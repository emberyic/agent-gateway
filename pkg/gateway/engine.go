package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/emberyic/agent-gateway/pkg/config"
)

// bufferPool 复用转发缓冲区，避免每个请求都新分配一块
type bufferPool struct {
	size int
	pool sync.Pool
}

func newBufferPool(size int) *bufferPool {
	return &bufferPool{
		size: size,
		pool: sync.Pool{
			New: func() any {
				buf := make([]byte, size)
				return &buf
			},
		},
	}
}

func (p *bufferPool) Get() []byte { return *p.pool.Get().(*[]byte) }

func (p *bufferPool) Put(b []byte) {
	if cap(b) < p.size { // 不是本池规格的缓冲区不回收
		return
	}
	p.pool.Put(&b)
}

// Engine 持有共享的连接池、缓冲区池，以及每条路由一个的反向代理
type Engine struct {
	transport *http.Transport
	buffers   *bufferPool
	proxies   map[string]*httputil.ReverseProxy
	log       *slog.Logger
	metrics   *Metrics
}

// NewEngine 为每条路由构建一个反向代理，共享同一个 Transport（连接池）
func NewEngine(routes []config.Route, responseHeaderTimeout time.Duration, trustProxy bool, log *slog.Logger, m *Metrics) (*Engine, error) {
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = 60 * time.Second
	}

	e := &Engine{
		transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 32, // 旧实现只有 10，对单个后端偏小
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true, // 转发不压缩，省 CPU；压缩交给外层
			// 只等响应头，不等响应体：流式接口因此可以放心用
			ResponseHeaderTimeout: responseHeaderTimeout,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
		},
		buffers: newBufferPool(32 * 1024),
		proxies: make(map[string]*httputil.ReverseProxy, len(routes)),
		log:     log,
		metrics: m,
	}

	for _, rt := range routes {
		proxy, err := e.buildProxy(rt, trustProxy)
		if err != nil {
			e.transport.CloseIdleConnections()
			return nil, err
		}
		e.proxies[rt.Path] = proxy
	}
	return e, nil
}

// Close 关闭空闲连接
func (e *Engine) Close() { e.transport.CloseIdleConnections() }

func (e *Engine) buildProxy(rt config.Route, trustProxy bool) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(rt.Backend)
	if err != nil {
		return nil, fmt.Errorf("路由 %s 的后端地址 %q 无效: %w", rt.Path, rt.Backend, err)
	}
	prefix := strings.TrimSuffix(rt.Path, "/")

	proxy := &httputil.ReverseProxy{
		Transport:  e.transport,
		BufferPool: e.buffers,
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out

			if rt.StripPrefix && prefix != "" {
				out.URL.Path = strings.TrimPrefix(out.URL.Path, prefix)
				if out.URL.Path == "" {
					out.URL.Path = "/"
				}
				out.URL.RawPath = ""
			}

			// SetURL 只改 scheme/host/path，RawQuery 原样保留（这正是旧实现丢掉的东西）
			pr.SetURL(target)

			if !trustProxy {
				// 不采信客户端自己带的 X-Forwarded-For，否则后端看到的是伪造的链路
				out.Header.Del("X-Forwarded-For")
			} else if prior := pr.In.Header.Values("X-Forwarded-For"); len(prior) > 0 {
				// 站在可信反代之后：Go 在用 Rewrite 时不会把入站 XFF 带到出站请求上
				out.Header["X-Forwarded-For"] = append([]string(nil), prior...)
			}
			out.Header.Set("X-Real-IP", clientIP(pr.In, trustProxy))
			pr.SetXForwarded() // 追加真实对端，并补 X-Forwarded-Host/Proto
			out.Header.Set("X-Forwarded-By", "my-gateway")
		},
		ErrorHandler:   e.errorHandler(rt),
		ModifyResponse: e.modifyResponse(rt),
	}
	if rt.Streaming {
		// -1 = 立即 flush，不等任何缓冲
		proxy.FlushInterval = -1
	}
	return proxy, nil
}

// modifyResponse 只动响应头，不碰 body
func (e *Engine) modifyResponse(rt config.Route) func(*http.Response) error {
	return func(res *http.Response) error {
		if isEventStream(res.Header.Get("Content-Type")) {
			// 保证中间层不缓存、不缓冲（前面还挂着 nginx 时，
			// 没有 X-Accel-Buffering: no，nginx 会把 SSE 攒起来才发）
			res.Header.Set("Cache-Control", "no-cache")
			res.Header.Set("X-Accel-Buffering", "no")
		}
		res.Header.Set("X-Gateway-Route", rt.Path) // 排查「到底走了哪条路由」用
		return nil
	}
}

// isEventStream 判断响应是不是 SSE（text/event-stream）
func isEventStream(contentType string) bool {
	if contentType == "" {
		return false
	}
	base, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.HasPrefix(strings.ToLower(contentType), "text/event-stream")
	}
	return base == "text/event-stream"
}

// errorHandler 把转发失败翻译成明确的 HTTP 语义
func (e *Engine) errorHandler(rt config.Route) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled):
			if e.log != nil {
				e.log.Debug("客户端断开，放弃转发", "route", rt.Path, "path", r.URL.Path)
			}
			return
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(r.Context().Err(), context.DeadlineExceeded):
			e.fail(w, r, rt, err, http.StatusGatewayTimeout, "backend_timeout", "后端响应超时")
		default:
			// ResponseHeaderTimeout、拨号超时这类错误不是 context 错误，
			// 但语义上同样是「等后端等超了」，统一回 504
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				e.fail(w, r, rt, err, http.StatusGatewayTimeout, "backend_timeout", "后端响应超时")
				return
			}
			e.fail(w, r, rt, err, http.StatusBadGateway, "bad_gateway", "后端不可用")
		}
	}
}

func (e *Engine) fail(w http.ResponseWriter, r *http.Request, rt config.Route, err error, status int, code, message string) {
	e.metrics.IncProxyErrors()
	if e.log != nil {
		e.log.Error("转发失败",
			"route", rt.Path,
			"path", r.URL.Path,
			"status", status,
			"err", err,
			"request_id", RequestIDFromContext(r.Context()),
		)
	}
	// 响应头已经发出去（比如流式传到一半后端挂了）：只能结束连接，
	// 补写状态码反而会把错误 JSON 混进 SSE 流里
	writeJSON(w, status, errorBody{Code: code, Message: message})
}
