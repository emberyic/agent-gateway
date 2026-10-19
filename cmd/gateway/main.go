// Command gateway 是网关入口：读配置 → 装配 → 起服务 → 优雅退出
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/emberyic/agent-gateway/pkg/config"
	"github.com/emberyic/agent-gateway/pkg/gateway"
)

func main() {
	configPath := flag.String("config", envOr("GATEWAY_CONFIG", "configs/gateway.yml"), "配置文件路径")
	flag.Parse()

	// 先用环境变量建 logger，好在读配置失败时也能记日志
	logger := newLogger(os.Getenv("GATEWAY_LOG_LEVEL"), os.Getenv("GATEWAY_LOG_FORMAT"))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("加载配置失败", "path", *configPath, "err", err)
		os.Exit(1)
	}
	if err := applyEnvOverrides(cfg); err != nil {
		logger.Error("环境变量覆盖失败", "err", err)
		os.Exit(1)
	}
	// 配置读出来了，按最终生效的日志设置重建 logger
	logger = newLogger(firstNonEmpty(os.Getenv("GATEWAY_LOG_LEVEL"), cfg.LogLevel),
		firstNonEmpty(os.Getenv("GATEWAY_LOG_FORMAT"), cfg.LogFormat))

	gw, err := gateway.New(cfg, logger)
	if err != nil {
		logger.Error("装配网关失败", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout.Duration(),
		IdleTimeout:       cfg.IdleTimeout.Duration(),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		// 刻意不设 WriteTimeout / ReadTimeout：它们按总时长算，会把 SSE 长连接切断
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("网关启动",
			"listen", cfg.Listen,
			"routes", routeSummary(cfg),
			"rate_limit_rps", cfg.RateLimit.RPS,
			"rate_limit_burst", cfg.RateLimit.Burst,
			"max_concurrent_streams", cfg.MaxConcurrentStreams,
			"trust_proxy", cfg.TrustProxy,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		logger.Error("网关异常退出", "err", err)
		gw.Close()
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅关闭", "timeout", cfg.ShutdownTimeout.Duration().String())
	}

	// Shutdown 会等在途请求结束；SSE 还挂着时会一直等到超时，所以要给它上限
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout.Duration())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("优雅关闭超时，强制结束", "err", err)
		_ = srv.Close()
	}
	gw.Close()
	logger.Info("网关已停止")
}

// applyEnvOverrides 用环境变量覆盖配置，容器里调参不用改文件、不用重构建镜像
func applyEnvOverrides(cfg *config.Config) error {
	if v := os.Getenv("GATEWAY_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("GATEWAY_RATE_LIMIT_RPS"); v != "" {
		rps, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("GATEWAY_RATE_LIMIT_RPS=%q 不是数字: %w", v, err)
		}
		cfg.RateLimit.RPS = rps
	}
	if v := os.Getenv("GATEWAY_RATE_LIMIT_BURST"); v != "" {
		burst, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("GATEWAY_RATE_LIMIT_BURST=%q 不是整数: %w", v, err)
		}
		cfg.RateLimit.Burst = burst
	}
	if v := os.Getenv("GATEWAY_MAX_STREAMS"); v != "" {
		maxStreams, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("GATEWAY_MAX_STREAMS=%q 不是整数: %w", v, err)
		}
		cfg.MaxConcurrentStreams = maxStreams
	}
	if v := os.Getenv("GATEWAY_TRUST_PROXY"); v != "" {
		trust, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GATEWAY_TRUST_PROXY=%q 不是布尔值: %w", v, err)
		}
		cfg.TrustProxy = trust
	}
	return cfg.Validate()
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

// routeSummary 把路由表压成启动日志用的一行摘要
func routeSummary(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Routes))
	for _, rt := range cfg.Routes {
		kind := "http"
		if rt.Streaming {
			kind = "sse"
		}
		suffix := ""
		if rt.StripPrefix {
			suffix = ", strip_prefix"
		}
		out = append(out, fmt.Sprintf("%s -> %s [%s%s]", rt.Path, rt.Backend, kind, suffix))
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
