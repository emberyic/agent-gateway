package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/emberyic/agent-gateway/pkg/config"
)

// errorBody 是所有错误响应的统一结构
// 与 agent-go 的 {"code","message"} 保持一致，前端只认一种错误形状
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON 写一个 JSON 响应
func writeJSON(w http.ResponseWriter, status int, body any) {
	if wroteHeader(w) {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// wroteHeader 判断响应是否已经开始（由 responseRecorder 提供）
func wroteHeader(w http.ResponseWriter) bool {
	if hw, ok := w.(interface{ WroteHeader() bool }); ok {
		return hw.WroteHeader()
	}
	return false
}

// route 是一条装配好的路由：配置 + 最终 handler（流式路由外面套了并发闸）
type route struct {
	cfg     config.Route
	handler http.Handler
}

// Gateway 把配置、转发引擎、限流和流式并发闸组装成一个 http.Handler
type Gateway struct {
	cfg     *config.Config
	engine  *Engine
	limiter *RateLimiter
	streams *StreamLimiter
	metrics *Metrics
	health  *HealthChecker
	log     *slog.Logger

	routes  []route // 按路径长度降序：最长前缀优先
	handler http.Handler
}

// New 按配置装配网关
func New(cfg *config.Config, log *slog.Logger) (*Gateway, error) {
	if cfg == nil {
		return nil, fmt.Errorf("配置为空")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	m := NewMetrics()
	engine, err := NewEngine(cfg.Routes, cfg.BackendResponseHeaderTimeout.Duration(), cfg.TrustProxy, log, m)
	if err != nil {
		return nil, err
	}

	g := &Gateway{
		cfg:     cfg,
		engine:  engine,
		limiter: NewRateLimiter(cfg.RateLimit.RPS, cfg.RateLimit.Burst, cfg.TrustProxy, m, log),
		streams: NewStreamLimiter(cfg.MaxConcurrentStreams, m),
		metrics: m,
		health:  NewHealthChecker(cfg.Health),
		log:     log,
	}

	routes := make([]route, 0, len(cfg.Routes))
	for _, rt := range cfg.Routes {
		proxy, ok := engine.proxies[rt.Path]
		if !ok {
			return nil, fmt.Errorf("路由 %s 没有对应的反向代理", rt.Path)
		}
		var h http.Handler = proxy
		if rt.Streaming {
			// 流式请求先过并发闸：满了就在这里 429，不会打到后端
			h = g.streams.Middleware(h)
		}
		routes = append(routes, route{cfg: rt, handler: h})
	}

	// 最长前缀优先：/api/agent 不会被 /api 抢走，"/" 兜底永远排最后
	sort.SliceStable(routes, func(i, j int) bool {
		return len(routes[i].cfg.Path) > len(routes[j].cfg.Path)
	})
	g.routes = routes
	g.handler = g.buildHandler()
	return g, nil
}

// Handler 返回可以直接挂到 http.Server 上的处理器
func (g *Gateway) Handler() http.Handler { return g.handler }

// Close 释放后台资源：限流清理 goroutine 与空闲连接
func (g *Gateway) Close() {
	g.limiter.Close()
	g.engine.Close()
}

// match 返回匹配到的路由下标，-1 表示没匹配上
func (g *Gateway) match(path string) int {
	for i := range g.routes {
		if strings.HasPrefix(path, g.routes[i].cfg.Path) {
			return i
		}
	}
	return -1
}

// routeLabel 给日志/指标用的稳定标签：匹配到的路由前缀，未匹配用 "<none>"
// 用路由前缀而不是原始路径，避免指标基数随 URL 数量无限膨胀
func (g *Gateway) routeLabel(path string) string {
	if isInternal(path) {
		return "<internal>"
	}
	if i := g.match(path); i >= 0 {
		return g.routes[i].cfg.Path
	}
	return "<none>"
}

// isInternal 判断是不是网关自己的端点
// 这些端点不参与限流，否则健康检查会被自己的限流打成 429
func isInternal(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/metrics":
		return true
	}
	return false
}

// buildHandler 按「外 → 内」组装中间件链
func (g *Gateway) buildHandler() http.Handler {
	limited := g.limiter.Middleware(http.HandlerFunc(g.dispatch))

	withInternal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isInternal(r.URL.Path) {
			g.serveInternal(w, r)
			return
		}
		limited.ServeHTTP(w, r)
	})

	recovered := withRecover(g.log, withInternal)
	logged := withAccessLog(g.log, g.metrics, g.routeLabel, recovered)
	return withRequestID(logged)
}

// dispatch 按最长前缀把请求交给对应路由的 handler
func (g *Gateway) dispatch(w http.ResponseWriter, r *http.Request) {
	i := g.match(r.URL.Path)
	if i < 0 {
		writeJSON(w, http.StatusNotFound, errorBody{
			Code:    "route_not_found",
			Message: "没有匹配到路由: " + r.URL.Path + "（网关自身端点: /healthz、/metrics）",
		})
		return
	}

	rt := g.routes[i]
	// 只有非流式路由才套总超时：流式路由配了 timeout 会在配置校验阶段直接报错，
	// 因为总超时会把 SSE 流从中间掐断
	if d := rt.cfg.Timeout.Duration(); d > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		r = r.WithContext(ctx)
	}
	rt.handler.ServeHTTP(w, r)
}

// serveInternal 处理网关自身的端点
func (g *Gateway) serveInternal(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		g.serveLiveness(w)
	case "/readyz":
		g.serveReadiness(w, r)
	case "/metrics":
		g.metrics.Handler().ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// localHealth 是网关自身的状态，不含任何下游探测
func (g *Gateway) localHealth() map[string]any {
	return map[string]any{
		"time":              time.Now().Format(time.RFC3339),
		"routes":            g.routePaths(),
		"active_requests":   g.metrics.ActiveRequests(),
		"streams_in_flight": g.metrics.StreamsInFlight(),
		"max_streams":       g.streams.Max(),
	}
}

// serveLiveness 是存活探针：只报网关自己，永远 200，绝不依赖下游
func (g *Gateway) serveLiveness(w http.ResponseWriter) {
	payload := g.localHealth()
	payload["status"] = "ok"
	writeJSON(w, http.StatusOK, payload)
}

// serveReadiness 是就绪探针：聚合「网关自身 + 上游（Agent 后端）」的状态
func (g *Gateway) serveReadiness(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	state := "ok"
	payload := g.localHealth()

	if g.health != nil {
		upstream := g.health.Check(r.Context())
		payload["upstream"] = upstream
		payload["upstream_url"] = g.health.URL()
		if !upstream.OK {
			state = "degraded"
			if g.cfg.Health.Required {
				status = http.StatusServiceUnavailable
			}
		}
	}

	payload["status"] = state
	writeJSON(w, status, payload)
}

func (g *Gateway) routePaths() []string {
	out := make([]string, 0, len(g.routes))
	for _, rt := range g.routes {
		out = append(out, rt.cfg.Path)
	}
	return out
}
