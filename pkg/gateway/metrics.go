package gateway

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Metrics 是网关的运行指标
type Metrics struct {
	requestsTotal   atomic.Int64
	activeRequests  atomic.Int64
	rateLimited     atomic.Int64
	streamRejected  atomic.Int64
	proxyErrors     atomic.Int64
	streamsInFlight atomic.Int64

	mu       sync.Mutex
	byRoute  map[string]int64
	byStatus map[string]int64
}

// NewMetrics 创建指标集合
func NewMetrics() *Metrics {
	return &Metrics{
		byRoute:  make(map[string]int64),
		byStatus: make(map[string]int64),
	}
}

// BeginRequest 记一次请求开始，返回的函数在请求结束时调用（传最终状态码）
func (m *Metrics) BeginRequest(route string) func(status int) {
	m.requestsTotal.Add(1)
	m.activeRequests.Add(1)
	m.mu.Lock()
	m.byRoute[route]++
	m.mu.Unlock()
	return func(status int) {
		m.activeRequests.Add(-1)
		m.mu.Lock()
		m.byStatus[strconv.Itoa(status)]++
		m.mu.Unlock()
	}
}

// IncRateLimited 记一次被 IP 限流拒绝
func (m *Metrics) IncRateLimited() { m.rateLimited.Add(1) }

// IncStreamRejected 记一次因流式并发已满被拒绝
func (m *Metrics) IncStreamRejected() { m.streamRejected.Add(1) }

// IncProxyErrors 记一次转发失败（含后端超时）
func (m *Metrics) IncProxyErrors() { m.proxyErrors.Add(1) }

// StreamsInFlight 返回当前在跑的流式请求数
func (m *Metrics) StreamsInFlight() int64 { return m.streamsInFlight.Load() }

// ActiveRequests 返回当前在处理的请求数
func (m *Metrics) ActiveRequests() int64 { return m.activeRequests.Load() }

// Handler 输出 Prometheus 文本格式（text/plain; version=0.0.4）
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		var b strings.Builder
		writeMetric(&b, "counter", "gateway_requests_total", "网关收到的请求总数", m.requestsTotal.Load())
		writeMetric(&b, "gauge", "gateway_active_requests", "当前正在处理的请求数", m.activeRequests.Load())
		writeMetric(&b, "counter", "gateway_rate_limited_total", "被 IP 限流拒绝的请求数", m.rateLimited.Load())
		writeMetric(&b, "counter", "gateway_stream_rejected_total", "因流式并发已满被拒绝的请求数", m.streamRejected.Load())
		writeMetric(&b, "counter", "gateway_proxy_errors_total", "转发后端失败的次数（含超时）", m.proxyErrors.Load())
		writeMetric(&b, "gauge", "gateway_streams_in_flight", "当前正在进行的流式请求数", m.streamsInFlight.Load())

		routes, statuses := m.snapshot()
		b.WriteString("# HELP gateway_requests_by_route_total 按路由前缀统计的请求数\n")
		b.WriteString("# TYPE gateway_requests_by_route_total counter\n")
		for _, route := range routes {
			fmt.Fprintf(&b, "gateway_requests_by_route_total{route=%q} %d\n", route.label, route.value)
		}
		b.WriteString("# HELP gateway_responses_by_status_total 按状态码统计的响应数\n")
		b.WriteString("# TYPE gateway_responses_by_status_total counter\n")
		for _, st := range statuses {
			fmt.Fprintf(&b, "gateway_responses_by_status_total{status=%q} %d\n", st.label, st.value)
		}

		_, _ = io.WriteString(w, b.String())
	})
}

// sample 是一个带标签的指标值
type sample struct {
	label string
	value int64
}

// snapshot 在锁内把 map 拷成有序切片，返回值可以安全地在锁外遍历
func (m *Metrics) snapshot() (routes, statuses []sample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	routes = make([]sample, 0, len(m.byRoute))
	for k, v := range m.byRoute {
		routes = append(routes, sample{label: k, value: v})
	}
	statuses = make([]sample, 0, len(m.byStatus))
	for k, v := range m.byStatus {
		statuses = append(statuses, sample{label: k, value: v})
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].label < routes[j].label })
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].label < statuses[j].label })
	return routes, statuses
}

func writeMetric(b *strings.Builder, kind, name, help string, value int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, kind, name, value)
}
