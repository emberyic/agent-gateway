package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// 解析客户端 IP
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := xff
			if i := strings.IndexByte(xff, ','); i >= 0 {
				first = xff[:i]
			}
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// 按客户端 IP 的令牌桶限流
type RateLimiter struct {
	rate       rate.Limit
	burst      int
	trustProxy bool
	idleTTL    time.Duration
	sweepEvery time.Duration
	metrics    *Metrics
	log        *slog.Logger

	mu       sync.RWMutex
	limiters map[string]*ipLimiter

	stop     chan struct{}
	stopOnce sync.Once
}

// ipLimiter 是单个 IP 的桶。lastSeen 用原子值，避免每次请求都要写锁
type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64 // UnixNano
}

// 创建限流器， rps <= 0 表示不启用（直接放行）
func NewRateLimiter(rps float64, burst int, trustProxy bool, m *Metrics, log *slog.Logger) *RateLimiter {
	if rps > 0 && burst <= 0 {
		burst = int(math.Ceil(rps))
	}
	rl := &RateLimiter{
		rate:       rate.Limit(rps),
		burst:      burst,
		trustProxy: trustProxy,
		idleTTL:    5 * time.Minute,
		sweepEvery: time.Minute,
		metrics:    m,
		log:        log,
		limiters:   make(map[string]*ipLimiter),
		stop:       make(chan struct{}),
	}
	if rl.Enabled() {
		go rl.sweepLoop()
	}
	return rl
}

// 报告限流是否启用
func (rl *RateLimiter) Enabled() bool {
	return rl != nil && rl.rate > 0 && rl.burst > 0
}

// 停止后台清理（可重复调用）
func (rl *RateLimiter) Close() {
	if rl == nil {
		return
	}
	rl.stopOnce.Do(func() { close(rl.stop) })
}

func (rl *RateLimiter) sweepLoop() {
	ticker := time.NewTicker(rl.sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stop:
			return
		case now := <-ticker.C:
			rl.sweep(now)
		}
	}
}

// sweep 清掉长时间没出现的 IP，避免 map 随访问过的 IP 数无限增长
func (rl *RateLimiter) sweep(now time.Time) {
	cutoff := now.Add(-rl.idleTTL).UnixNano()
	rl.mu.Lock()
	for ip, l := range rl.limiters {
		if l.lastSeen.Load() < cutoff {
			delete(rl.limiters, ip)
		}
	}
	rl.mu.Unlock()
}

// limiterFor 取（必要时创建）某个 IP 的限流器
func (rl *RateLimiter) limiterFor(ip string) *ipLimiter {
	now := time.Now().UnixNano()

	rl.mu.RLock()
	l, ok := rl.limiters[ip]
	rl.mu.RUnlock()
	if ok {
		l.lastSeen.Store(now)
		return l
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()
	if l, ok = rl.limiters[ip]; ok { // 双检：可能已被并发创建
		l.lastSeen.Store(now)
		return l
	}
	l = &ipLimiter{limiter: rate.NewLimiter(rl.rate, rl.burst)}
	l.lastSeen.Store(now)
	rl.limiters[ip] = l
	return l
}

// nextTokenDelay 返回「还要等多久才有下一个令牌」
// Reserve + Cancel 询问，不会真的消耗令牌
func (rl *RateLimiter) nextTokenDelay(l *ipLimiter) time.Duration {
	res := l.limiter.Reserve()
	delay := res.Delay()
	res.Cancel()
	return delay
}

// 把等待时长换算成 Retry-After 的整数秒，夹在 [1, 60]
func retryAfterSeconds(d time.Duration) int {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	if secs > 60 {
		secs = 60
	}
	return secs
}

// 包装 handler 做 IP 限流
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	if !rl.Enabled() {
		return next
	}
	limitLabel := strconv.FormatFloat(float64(rl.rate), 'f', -1, 64)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, rl.trustProxy)
		l := rl.limiterFor(ip)

		if !l.limiter.Allow() {
			rl.metrics.IncRateLimited()
			retryAfter := retryAfterSeconds(rl.nextTokenDelay(l))
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.Header().Set("X-RateLimit-Limit", limitLabel)
			w.Header().Set("X-RateLimit-Remaining", "0")
			if rl.log != nil {
				rl.log.Warn("请求被限流",
					"ip", ip,
					"path", r.URL.Path,
					"retry_after_s", retryAfter,
					"request_id", RequestIDFromContext(r.Context()),
				)
			}
			writeJSON(w, http.StatusTooManyRequests, errorBody{
				Code:    "rate_limited",
				Message: "请求过于频繁，请稍后重试",
			})
			return
		}

		w.Header().Set("X-RateLimit-Limit", limitLabel)
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(int(l.limiter.Tokens())))
		next.ServeHTTP(w, r)
	})
}

// StreamLimiter 限制同时在跑的流式（SSE）请求数
type StreamLimiter struct {
	max     int
	slots   chan struct{}
	metrics *Metrics
}

// NewStreamLimiter 创建并发闸。max <= 0 表示不限
func NewStreamLimiter(max int, m *Metrics) *StreamLimiter {
	sl := &StreamLimiter{max: max, metrics: m}
	if max > 0 {
		sl.slots = make(chan struct{}, max)
	}
	return sl
}

// 返回配置的并发上限
func (sl *StreamLimiter) Max() int {
	if sl == nil {
		return 0
	}
	return sl.max
}

// Acquire 非阻塞地占用一个槽位，返回释放函数
func (sl *StreamLimiter) Acquire() (func(), bool) {
	if sl == nil || sl.max <= 0 {
		return func() {}, true
	}
	select {
	case sl.slots <- struct{}{}:
		sl.metrics.streamsInFlight.Add(1)
		var once sync.Once
		return func() {
			once.Do(func() { // 释放必须幂等
				<-sl.slots
				sl.metrics.streamsInFlight.Add(-1)
			})
		}, true
	default:
		return nil, false
	}
}

// 包装流式路由的 handler
func (sl *StreamLimiter) Middleware(next http.Handler) http.Handler {
	if sl == nil || sl.max <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := sl.Acquire()
		if !ok {
			sl.metrics.IncStreamRejected()
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, errorBody{
				Code:    "too_many_streams",
				Message: "并发对话数已达上限，请稍后重试",
			})
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const requestIDKey ctxKey = iota

// 取请求 ID，没有则返回空串
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// 给每个请求打一个短 ID：写进响应头和 context，日志用它串联
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// 兜住 handler 里的 panic：回 500 而不是让连接凭空断掉
func withRecover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			if log != nil {
				log.Error("handler 发生 panic",
					"err", rec,
					"path", r.URL.Path,
					"request_id", RequestIDFromContext(r.Context()),
				)
			}
			// 流式传输中途崩了就只记日志，不再补状态码
			writeJSON(w, http.StatusInternalServerError, errorBody{
				Code:    "internal_error",
				Message: "网关内部错误",
			})
		}()
		next.ServeHTTP(w, r)
	})
}

// responseRecorder 记录状态码与响应字节数
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (rr *responseRecorder) WriteHeader(code int) {
	if rr.wrote {
		return
	}
	rr.status = code
	rr.wrote = true
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	if !rr.wrote {
		rr.WriteHeader(http.StatusOK)
	}
	n, err := rr.ResponseWriter.Write(b)
	rr.bytes += int64(n)
	return n, err
}

// Flush 透传到底层 ResponseWriter（SSE 的生命线）
func (rr *responseRecorder) Flush() {
	if !rr.wrote {
		rr.WriteHeader(http.StatusOK)
	}
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能找到底层 writer
func (rr *responseRecorder) Unwrap() http.ResponseWriter { return rr.ResponseWriter }

// WroteHeader 报告响应头是否已经发出（决定还能不能回错误 JSON）
func (rr *responseRecorder) WroteHeader() bool { return rr.wrote }

// Status 返回已写出的状态码，没写过时视为 200
func (rr *responseRecorder) Status() int {
	if rr.status == 0 {
		return http.StatusOK
	}
	return rr.status
}

// 记录每个请求的结果
func withAccessLog(log *slog.Logger, m *Metrics, routeOf func(string) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		route := routeOf(r.URL.Path)
		done := m.BeginRequest(route)

		rr := &responseRecorder{ResponseWriter: w}
		defer func() {
			done(rr.Status())
			if log != nil {
				log.Info("请求完成",
					"method", r.Method,
					"path", r.URL.Path,
					"route", route,
					"status", rr.Status(),
					"bytes", rr.bytes,
					"duration_ms", time.Since(start).Milliseconds(),
					"peer_ip", clientIP(r, false), // 日志记真实对端，不采用可伪造的 XFF
					"request_id", RequestIDFromContext(r.Context()),
				)
			}
		}()

		next.ServeHTTP(rr, r)
	})
}
