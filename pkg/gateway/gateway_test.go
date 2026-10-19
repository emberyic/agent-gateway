package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emberyic/agent-gateway/pkg/config"
)

// newTestGateway 装配一个测试用网关，返回指向它的 httptest server
func newTestGateway(t *testing.T, mutate func(*config.Config)) (*httptest.Server, *Gateway) {
	t.Helper()
	return newTestGatewayLogged(t, slog.New(slog.NewTextHandler(io.Discard, nil)), mutate)
}

// newTestGatewayLogged 同上，但允许传入 logger 以便断言日志内容
func newTestGatewayLogged(t *testing.T, logger *slog.Logger, mutate func(*config.Config)) (*httptest.Server, *Gateway) {
	t.Helper()

	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	mutate(cfg)

	gw, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("装配网关失败: %v", err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(func() {
		srv.Close()
		gw.Close()
	})
	return srv, gw
}

// syncBuffer 是并发安全的日志缓冲区（handler goroutine 写、测试 goroutine 读）
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func doGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	return resp
}

// 旧实现转发时用的是 targetURL + r.URL.Path，把 query 整段丢了
// 对 /chat/stream?question=...&session_id=... 这种接口，等于后端永远收不到问题
func TestProxyPreservesPathQueryAndHardensForwardedHeaders(t *testing.T) {
	type seen struct {
		path, query, xff, realIP string
	}
	var got seen
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			xff:    r.Header.Get("X-Forwarded-For"),
			realIP: r.Header.Get("X-Real-IP"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.Routes = []config.Route{{Path: "/api", Backend: backend.URL}}
	})

	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/api/echo?question=%E4%BD%A0%E5%A5%BD&session_id=user_001", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	// 模拟客户端伪造 X-Forwarded-For：默认配置（trust_proxy: false）不应采信
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer resp.Body.Close()

	if got.path != "/api/echo" {
		t.Errorf("后端收到的路径 = %q，期望 /api/echo", got.path)
	}
	const wantQuery = "question=%E4%BD%A0%E5%A5%BD&session_id=user_001"
	if got.query != wantQuery {
		t.Errorf("后端收到的 query = %q，期望 %q（网关必须原样保留 query）", got.query, wantQuery)
	}
	if got.xff != "127.0.0.1" {
		t.Errorf("默认不采信客户端 XFF，后端应只看到真实对端 127.0.0.1，实际 %q", got.xff)
	}
	if got.realIP != "127.0.0.1" {
		t.Errorf("X-Real-IP = %q，期望 127.0.0.1", got.realIP)
	}
	// 网关在响应上加的路由标记，用于排查「这个请求到底走了哪条路由」
	if resp.Header.Get("X-Gateway-Route") != "/api" {
		t.Errorf("响应头 X-Gateway-Route = %q，期望 /api", resp.Header.Get("X-Gateway-Route"))
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("响应应带 X-Request-Id")
	}
}

func TestTrustProxyAcceptsForwardedFor(t *testing.T) {
	var gotXFF, gotRealIP string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotRealIP = r.Header.Get("X-Real-IP")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.TrustProxy = true
		cfg.Routes = []config.Route{{Path: "/api", Backend: backend.URL}}
	})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/x", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer resp.Body.Close()

	if gotRealIP != "1.2.3.4" {
		t.Errorf("trust_proxy 打开后 X-Real-IP 应取 XFF 第一段，实际 %q", gotRealIP)
	}
	// 完整链路 = 客户端传来的链 + 网关看到的真实对端
	const wantChain = "1.2.3.4, 10.0.0.1, 127.0.0.1"
	if gotXFF != wantChain {
		t.Errorf("X-Forwarded-For = %q，期望 %q", gotXFF, wantChain)
	}
}

// 网关绝不能把 SSE 攒起来一次性发：后端 flush 一帧，客户端就必须立刻收到一帧
func TestStreamingIsFlushedFrameByFrame(t *testing.T) {
	firstFrameFlushed := make(chan struct{})
	allowRest := make(chan struct{})
	var releaseOnce sync.Once
	releaseRest := func() { releaseOnce.Do(func() { close(allowRest) }) }

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("后端 ResponseWriter 不支持 flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: 第一帧\n\n")
		flusher.Flush()
		close(firstFrameFlushed)

		select {
		case <-allowRest:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: 第二帧\n\n")
		flusher.Flush()
	}))
	defer backend.Close()

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.Routes = []config.Route{{Path: "/chat/stream", Backend: backend.URL, Streaming: true}}
	})
	defer releaseRest()

	resp, err := srv.Client().Get(srv.URL + "/chat/stream")
	if err != nil {
		t.Fatalf("请求网关失败: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}

	select {
	case <-firstFrameFlushed:
	case <-time.After(2 * time.Second):
		t.Fatal("后端第一帧都没写出去")
	}

	lineCh := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil {
			lineCh <- "读取失败: " + err.Error()
			return
		}
		lineCh <- line
	}()

	select {
	case line := <-lineCh:
		if !strings.Contains(line, "第一帧") {
			t.Fatalf("客户端收到 %q，期望第一帧", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("网关把 SSE 缓冲住了：后端已 flush 第一帧，客户端却读不到")
	}
}

// 并发闸：槽位满了必须在写响应头之前干净地回 429
func TestStreamAdmissionRejectsWhenFull(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("后端 ResponseWriter 不支持 flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 必须先写一帧并 flush，否则客户端的 Get 会一直等首字节
		_, _ = io.WriteString(w, "data: 开始\n\n")
		flusher.Flush()

		select {
		case entered <- struct{}{}:
		default:
		}

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: 结束\n\n")
		flusher.Flush()
	}))
	defer backend.Close()

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.MaxConcurrentStreams = 1
		cfg.Routes = []config.Route{{Path: "/chat/slow", Backend: backend.URL, Streaming: true}}
	})

	first, err := srv.Client().Get(srv.URL + "/chat/slow")
	if err != nil {
		t.Fatalf("第一个流请求失败: %v", err)
	}
	defer first.Body.Close()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("第一个流请求没有进到后端")
	}

	second, err := srv.Client().Get(srv.URL + "/chat/slow")
	if err != nil {
		t.Fatalf("第二个流请求失败: %v", err)
	}
	defer second.Body.Close()

	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("并发已满应回 429，实际 %d", second.StatusCode)
	}
	if second.Header.Get("Retry-After") == "" {
		t.Error("429 响应应带 Retry-After")
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(second.Body).Decode(&body); err != nil {
		t.Fatalf("解析 429 响应失败: %v", err)
	}
	if body.Code != "too_many_streams" {
		t.Errorf("错误码 = %q，期望 too_many_streams", body.Code)
	}

	releaseFirst()
}

func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.RateLimit = config.RateLimit{RPS: 1, Burst: 1}
		cfg.Routes = []config.Route{{Path: "/api", Backend: backend.URL}}
	})

	first, err := http.Get(srv.URL + "/api/x")
	if err != nil {
		t.Fatalf("第一个请求失败: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("第一个请求应通过，实际 %d", first.StatusCode)
	}
	if first.Header.Get("X-RateLimit-Limit") == "" {
		t.Error("通过时也应带 X-RateLimit-Limit")
	}

	second, err := http.Get(srv.URL + "/api/x")
	if err != nil {
		t.Fatalf("第二个请求失败: %v", err)
	}
	defer second.Body.Close()

	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("桶已空应回 429，实际 %d", second.StatusCode)
	}
	if second.Header.Get("Retry-After") == "" {
		t.Error("429 响应应带 Retry-After")
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(second.Body).Decode(&body); err != nil {
		t.Fatalf("解析 429 响应失败: %v", err)
	}
	if body.Code != "rate_limited" {
		t.Errorf("错误码 = %q，期望 rate_limited", body.Code)
	}
}

// 响应头超时只应约束「后端多久开始回响应头」，不能约束响应体传多久
// 这正是旧实现 http.Client.Timeout: 30s 的病根 —— 它会把长 SSE 流切断
func TestResponseHeaderTimeoutDoesNotCutStreamBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-header" {
			time.Sleep(500 * time.Millisecond) // 故意拖过 200ms 的响应头超时
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("后端 ResponseWriter 不支持 flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ { // 响应体总共持续约 1.2 秒，远超 200ms
			_, _ = io.WriteString(w, "data: tick\n\n")
			flusher.Flush()
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer backend.Close()

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.BackendResponseHeaderTimeout = config.Duration(200 * time.Millisecond)
		cfg.Routes = []config.Route{
			{Path: "/slow-header", Backend: backend.URL, Timeout: config.Duration(5 * time.Second)},
			{Path: "/long-body", Backend: backend.URL, Streaming: true},
		}
	})

	slow := doGet(t, srv.URL+"/slow-header")
	slow.Body.Close()
	if slow.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("等响应头超时应回 504，实际 %d", slow.StatusCode)
	}

	long := doGet(t, srv.URL+"/long-body")
	defer long.Body.Close()
	raw, err := io.ReadAll(long.Body)
	if err != nil {
		t.Fatalf("读流失败: %v", err)
	}
	if got := strings.Count(string(raw), "data: tick"); got != 3 {
		t.Errorf("应完整收到 3 帧（响应体不受响应头超时约束），实际 %d 帧", got)
	}
}

func TestLongestPrefixWinsAndStripPrefixWorks(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	record := func(name string, r *http.Request) {
		mu.Lock()
		seen[name] = r.URL.Path
		mu.Unlock()
	}

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("agent", r)
		w.WriteHeader(http.StatusOK)
	}))
	defer agent.Close()

	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record("legacy", r)
		w.WriteHeader(http.StatusOK)
	}))
	defer legacy.Close()

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		// 故意打乱顺序：兜底路由写在最前面，靠「最长前缀优先」保证正确性
		cfg.Routes = []config.Route{
			{Path: "/", Backend: agent.URL, Timeout: config.Duration(30 * time.Second)},
			{Path: "/api/agent", Backend: agent.URL, StripPrefix: true},
			{Path: "/api", Backend: legacy.URL},
		}
	})

	doGet(t, srv.URL+"/api/agent/chat").Body.Close()
	if seen["agent"] != "/chat" {
		t.Errorf("strip_prefix 后后端应收到 /chat，实际 %q", seen["agent"])
	}

	doGet(t, srv.URL+"/api/other").Body.Close()
	if seen["legacy"] != "/api/other" {
		t.Errorf("更短的 /api 路由应命中 legacy，实际 %q", seen["legacy"])
	}

	doGet(t, srv.URL+"/anything").Body.Close()
	if seen["agent"] != "/anything" {
		t.Errorf("兜底路由应命中 agent，实际 %q", seen["agent"])
	}
}

func TestUnmatchedRouteReturns404JSONAndInternalEndpointsWork(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.Routes = []config.Route{{Path: "/api", Backend: backend.URL}}
	})

	notFound := doGet(t, srv.URL+"/nope")
	defer notFound.Body.Close()
	if notFound.StatusCode != http.StatusNotFound {
		t.Fatalf("未匹配路由应回 404，实际 %d", notFound.StatusCode)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(notFound.Body).Decode(&body); err != nil {
		t.Fatalf("404 应回 JSON: %v", err)
	}
	if body.Code != "route_not_found" {
		t.Errorf("错误码 = %q，期望 route_not_found", body.Code)
	}

	// 网关自身端点必须优先于路由匹配，也不该被 IP 限流挡住
	health := doGet(t, srv.URL+"/healthz")
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("/healthz 应回 200，实际 %d", health.StatusCode)
	}

	metrics := doGet(t, srv.URL+"/metrics")
	defer metrics.Body.Close()
	raw, err := io.ReadAll(metrics.Body)
	if err != nil {
		t.Fatalf("读指标失败: %v", err)
	}
	for _, want := range []string{"gateway_requests_total", "gateway_streams_in_flight"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("/metrics 应包含 %s", want)
		}
	}
}

// 访问日志必须带上请求 ID，而且不能把用户的问题（在 query 里）写进日志
func TestAccessLogCarriesRequestIDAndOmitsQuery(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	logs := &syncBuffer{}
	srv, _ := newTestGatewayLogged(t,
		slog.New(slog.NewTextHandler(logs, nil)),
		func(cfg *config.Config) {
			cfg.Routes = []config.Route{{Path: "/api", Backend: backend.URL}}
		})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/x?question=secret-question", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("X-Request-Id", "req-fixed-123")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()

	// 访问日志在 handler 返回后才写，与客户端收到响应之间没有顺序保证，轮询等一下
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "请求完成") {
		time.Sleep(10 * time.Millisecond)
	}

	got := logs.String()
	if !strings.Contains(got, "请求完成") {
		t.Fatalf("没有等到访问日志: %q", got)
	}
	if !strings.Contains(got, "request_id=req-fixed-123") {
		t.Errorf("访问日志应带上请求 ID，实际: %q", got)
	}
	if strings.Contains(got, "secret-question") {
		t.Errorf("访问日志不应记录 query（里面有用户输入），实际: %q", got)
	}
}

// 存活探针 /healthz 只报网关自己（永远 200，绝不依赖下游）；就绪探针 /readyz 才聚合上游
// —— 否则后端一挂，编排会把「还能正常转发」的网关一起重启掉
func TestLivenessStaysLocalAndReadinessAggregatesUpstream(t *testing.T) {
	var upstreamHealthy atomic.Bool
	upstreamHealthy.Store(true)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if upstreamHealthy.Load() {
			_, _ = io.WriteString(w, `{"status":"ok","checks":{"memory":"ok"}}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"status":"unhealthy","checks":{"memory":"unhealthy"}}`)
	}))
	defer backend.Close()

	type healthBody struct {
		Status   string `json:"status"`
		Upstream struct {
			OK        bool   `json:"ok"`
			Status    int    `json:"status"`
			LatencyMS int64  `json:"latency_ms"`
			Error     string `json:"error"`
			Body      string `json:"body"`
		} `json:"upstream"`
	}

	decode := func(resp *http.Response) healthBody {
		t.Helper()
		defer resp.Body.Close()
		var body healthBody
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("健康检查响应不是预期 JSON: %v", err)
		}
		return body
	}

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.Health = config.HealthConfig{
			UpstreamURL: backend.URL + "/healthz",
			Timeout:     config.Duration(time.Second),
			CacheTTL:    0, // 直接探，免得缓存掩盖状态切换
			Required:    true,
		}
		cfg.Routes = []config.Route{{Path: "/api", Backend: backend.URL}}
	})

	// 上游健康：两个探针都 200
	live := doGet(t, srv.URL+"/healthz")
	liveBody := decode(live)
	if live.StatusCode != http.StatusOK || liveBody.Status != "ok" {
		t.Fatalf("/healthz 应 200/ok，实际 %d/%q", live.StatusCode, liveBody.Status)
	}

	resp := doGet(t, srv.URL+"/readyz")
	body := decode(resp)
	if resp.StatusCode != http.StatusOK || body.Status != "ok" || !body.Upstream.OK {
		t.Fatalf("上游健康时 /readyz 应 200/ok，实际 %d/%q/upstream.ok=%v", resp.StatusCode, body.Status, body.Upstream.OK)
	}
	if !strings.Contains(body.Upstream.Body, "checks") {
		t.Errorf("应把上游的响应体带回来，实际 %q", body.Upstream.Body)
	}

	// 上游不健康：/healthz 仍然 200（存活），/readyz 才 503 + degraded
	upstreamHealthy.Store(false)

	live = doGet(t, srv.URL+"/healthz")
	liveBody = decode(live)
	if live.StatusCode != http.StatusOK || liveBody.Status != "ok" {
		t.Errorf("上游不健康时 /healthz 仍应 200/ok（存活探针不能依赖下游），实际 %d/%q", live.StatusCode, liveBody.Status)
	}

	resp = doGet(t, srv.URL+"/readyz")
	body = decode(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("required: true 时上游不健康 /readyz 应回 503，实际 %d", resp.StatusCode)
	}
	if body.Status != "degraded" {
		t.Errorf("status 应为 degraded，实际 %q", body.Status)
	}
	if body.Upstream.Error == "" {
		t.Error("应把上游不健康的原因带回来")
	}

	// required: false → 状态码不受影响，但结论要标出来
	soft, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.Health = config.HealthConfig{
			UpstreamURL: backend.URL + "/healthz",
			Timeout:     config.Duration(time.Second),
			CacheTTL:    0,
			Required:    false,
		}
		cfg.Routes = []config.Route{{Path: "/api", Backend: backend.URL}}
	})
	resp = doGet(t, soft.URL+"/readyz")
	body = decode(resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("required: false 时状态码应保持 200，实际 %d", resp.StatusCode)
	}
	if body.Status != "degraded" {
		t.Errorf("status 仍应为 degraded，实际 %q", body.Status)
	}
}

// 就绪探针不参与限流，探测放大只能靠缓存挡住：TTL 内重复请求不应再打后端
func TestReadinessProbeIsCached(t *testing.T) {
	var probes atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer backend.Close()

	srv, _ := newTestGateway(t, func(cfg *config.Config) {
		cfg.Health = config.HealthConfig{
			UpstreamURL: backend.URL + "/healthz",
			Timeout:     config.Duration(time.Second),
			CacheTTL:    config.Duration(5 * time.Second),
			Required:    true,
		}
		cfg.Routes = []config.Route{{Path: "/api", Backend: backend.URL}}
	})

	type readyBody struct {
		Upstream struct {
			OK     bool `json:"ok"`
			Cached bool `json:"cached"`
		} `json:"upstream"`
	}

	first := doGet(t, srv.URL+"/readyz")
	var firstBody readyBody
	if err := json.NewDecoder(first.Body).Decode(&firstBody); err != nil {
		t.Fatalf("解析 /readyz 失败: %v", err)
	}
	first.Body.Close()
	if firstBody.Upstream.Cached {
		t.Error("第一次探测不应来自缓存")
	}

	second := doGet(t, srv.URL+"/readyz")
	var secondBody readyBody
	if err := json.NewDecoder(second.Body).Decode(&secondBody); err != nil {
		t.Fatalf("解析 /readyz 失败: %v", err)
	}
	second.Body.Close()
	if !secondBody.Upstream.Cached {
		t.Error("TTL 内的第二次探测应命中缓存（upstream.cached=true）")
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("后端应只被探测 1 次（缓存生效），实际 %d 次", got)
	}
}
