package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emberyic/agent-gateway/pkg/config"
)

// maxUpstreamBody 探测上游时最多读多少字节（只用来带回失败原因，不做转发）
const maxUpstreamBody = 8 * 1024

// UpstreamHealth 是一次上游健康探测的结论
type UpstreamHealth struct {
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Cached    bool   `json:"cached,omitempty"` // 结论来自缓存（没真发探测）
	Error     string `json:"error,omitempty"`
	Body      string `json:"body,omitempty"` // 上游 2xx 时的响应体
}

// HealthChecker 探测上游（Agent 后端）的健康检查地址
type HealthChecker struct {
	url      string
	timeout  time.Duration
	cacheTTL time.Duration
	client   *http.Client

	mu       sync.Mutex
	cached   UpstreamHealth
	cachedAt time.Time
}

// NewHealthChecker 创建探测器；未配置 upstream_url 时返回 nil（不探测）
func NewHealthChecker(cfg config.HealthConfig) *HealthChecker {
	if strings.TrimSpace(cfg.UpstreamURL) == "" {
		return nil
	}
	timeout := cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	cacheTTL := cfg.CacheTTL.Duration()
	if cacheTTL < 0 {
		cacheTTL = 0
	}
	return &HealthChecker{
		url:      cfg.UpstreamURL,
		timeout:  timeout,
		cacheTTL: cacheTTL,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DisableKeepAlives: true, // 探测是低频动作，别占着连接池
			},
		},
	}
}

// URL 返回被探测的地址（健康报告里回显，方便确认探的是谁）
func (h *HealthChecker) URL() string {
	if h == nil {
		return ""
	}
	return h.url
}

// Check 返回一次上游健康结论，带 TTL 缓存（高频探针不至于把压力放大到后端）
func (h *HealthChecker) Check(ctx context.Context) UpstreamHealth {
	if h.cacheTTL > 0 {
		h.mu.Lock()
		if !h.cachedAt.IsZero() && time.Since(h.cachedAt) < h.cacheTTL {
			result := h.cached
			result.Cached = true
			h.mu.Unlock()
			return result
		}
		h.mu.Unlock()
	}

	result := h.probe(ctx)

	if h.cacheTTL > 0 {
		h.mu.Lock()
		h.cached = result
		h.cachedAt = time.Now()
		h.mu.Unlock()
	}
	return result
}

// probe 真发一次探测；任何失败都变成 UpstreamHealth 里的字段，不返回 error
func (h *HealthChecker) probe(ctx context.Context) UpstreamHealth {
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return UpstreamHealth{LatencyMS: time.Since(start).Milliseconds(), Error: err.Error()}
	}
	req.Header.Set("User-Agent", "my-gateway-healthcheck")

	resp, err := h.client.Do(req)
	if err != nil {
		return UpstreamHealth{LatencyMS: time.Since(start).Milliseconds(), Error: err.Error()}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	result := UpstreamHealth{
		Status:    resp.StatusCode,
		LatencyMS: time.Since(start).Milliseconds(),
		OK:        resp.StatusCode >= 200 && resp.StatusCode < 300,
	}
	if result.OK {
		result.Body = strings.TrimSpace(string(body))
	} else {
		// 非 2xx：把上游自报的原因带回来（截断，别把一堆 HTML 塞进健康报告）
		result.Error = fmt.Sprintf("上游返回 %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), 200))
	}
	return result
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
