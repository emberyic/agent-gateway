// Package config 负责网关的配置加载、默认值与校验
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 是 YAML 里的时长字段：既支持 "30s" / "1m30s" / "500ms" 这类字符串
type Duration time.Duration

// Duration 返回标准库时长
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalYAML 实现 yaml.Unmarshaler
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		// 少数 YAML 标量可能无法直接解成字符串，退一步按数字（秒）处理
		var secs float64
		if err2 := value.Decode(&secs); err2 == nil {
			*d = Duration(time.Duration(secs * float64(time.Second)))
			return nil
		}
		return fmt.Errorf("时长字段格式错误（应为 \"30s\" 或秒数）: %w", err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" { // 字段留空 => 0，交给默认值逻辑
		*d = 0
		return nil
	}
	if secs, err := strconv.ParseFloat(raw, 64); err == nil {
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("无法解析时长 %q（示例: 30s、1m30s、500ms）: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

// HealthConfig 描述网关 /readyz 里「上游（Agent 后端）」那一半的探测
type HealthConfig struct {
	// UpstreamURL 是 Agent 后端的健康检查地址，如 http://127.0.0.1:8000/healthz
	// 留空则 /readyz 也只报网关自身状态（不探测任何上游）
	UpstreamURL string `yaml:"upstream_url"`

	// Timeout 是单次探测上游的超时。探测失败只影响这次响应，不会阻塞其他请求
	Timeout Duration `yaml:"timeout"`

	// CacheTTL 是探测结果的缓存时长，0 表示每次都真探
	CacheTTL Duration `yaml:"cache_ttl"`

	// Required 为 true 时，上游不健康会让网关 /readyz 返回 503
	// 只想看不想影响编排（比如后端还在灰度）时设为 false
	Required bool `yaml:"required"`
}

// Route 是一条转发规则。按路径前缀匹配，长路径优先
type Route struct {
	// Path 是匹配前缀，必须以 / 开头；"/" 表示兜底路由
	Path string `yaml:"path"`

	// Backend 是后端地址，如 http://127.0.0.1:8000
	Backend string `yaml:"backend"`

	// StripPrefix 为 true 时把 Path 从转发路径里去掉：
	// /api/agent/chat → 后端收到 /chat
	StripPrefix bool `yaml:"strip_prefix"`

	// Streaming 标记这条路由是 SSE / 流式接口：立即 flush + 计入并发闸 + 禁止配 timeout
	Streaming bool `yaml:"streaming"`

	// Timeout 是这条路由的总超时，0 表示不限时
	// 只有非流式路由能设置它
	Timeout Duration `yaml:"timeout"`
}

// RateLimit 是按客户端 IP 的令牌桶参数
type RateLimit struct {
	// RPS 是每秒补充的令牌数；0 表示关闭 IP 限流
	RPS float64 `yaml:"rps"`

	// Burst 是桶容量，即允许的瞬时突发量
	Burst int `yaml:"burst"`
}

// Config 是网关的完整配置
type Config struct {
	// Listen 是监听地址，如 0.0.0.0:8080
	Listen string `yaml:"listen"`

	// Routes 是转发规则表，至少一条
	Routes []Route `yaml:"routes"`

	// RateLimit 是按 IP 的令牌桶限流
	RateLimit RateLimit `yaml:"rate_limit"`

	// MaxConcurrentStreams 是同时在跑的流式请求上限，0 表示不限
	MaxConcurrentStreams int `yaml:"max_concurrent_streams"`

	// TrustProxy 为 true 时才采信 X-Forwarded-For 来识别客户端 IP
	TrustProxy bool `yaml:"trust_proxy"`

	// ReadHeaderTimeout 是读请求头的上限。设 0 会用默认值
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`

	// IdleTimeout 是 keep-alive 空闲连接的超时
	IdleTimeout Duration `yaml:"idle_timeout"`

	// ShutdownTimeout 是收到退出信号后等待在途请求结束的上限
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`

	// BackendResponseHeaderTimeout 是「等后端开始回响应头」的上限
	BackendResponseHeaderTimeout Duration `yaml:"backend_response_header_timeout"`

	// Health 是 /healthz 里对上游的探测配置
	Health HealthConfig `yaml:"health"`

	// LogLevel 取 debug / info / warn / error
	LogLevel string `yaml:"log_level"`

	// LogFormat 取 text（人读）或 json（机器读）
	LogFormat string `yaml:"log_format"`
}

// Default 返回默认配置。Load 会先铺一份默认值再反序列化，
// 因此配置文件里没写的字段自动落到这里的值
func Default() *Config {
	return &Config{
		Listen:                       "0.0.0.0:8080",
		RateLimit:                    RateLimit{RPS: 50, Burst: 100},
		MaxConcurrentStreams:         64,
		ReadHeaderTimeout:            Duration(10 * time.Second),
		IdleTimeout:                  Duration(120 * time.Second),
		ShutdownTimeout:              Duration(15 * time.Second),
		BackendResponseHeaderTimeout: Duration(60 * time.Second),
		Health: HealthConfig{
			Timeout:  Duration(2 * time.Second),
			CacheTTL: Duration(1 * time.Second),
			Required: true, // 默认：上游不健康 → 网关 /readyz 报 503
		},
		LogLevel:  "info",
		LogFormat: "text",
	}
}

// Load 读取、补默认值并校验配置文件
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadConfig 是 Load 的旧名字，保留以免破坏已有调用
func LoadConfig(path string) (*Config, error) { return Load(path) }

// applyDefaults 补齐零值字段。显式写成 0 有时是有意义的
func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8080"
	}
	if c.RateLimit.RPS > 0 && c.RateLimit.Burst <= 0 {
		burst := int(c.RateLimit.RPS)
		if c.RateLimit.RPS > float64(burst) {
			burst++
		}
		if burst < 1 {
			burst = 1
		}
		c.RateLimit.Burst = burst
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = Duration(10 * time.Second)
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = Duration(120 * time.Second)
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = Duration(15 * time.Second)
	}
	if c.BackendResponseHeaderTimeout <= 0 {
		c.BackendResponseHeaderTimeout = Duration(60 * time.Second)
	}
	if c.Health.UpstreamURL != "" && c.Health.Timeout <= 0 {
		c.Health.Timeout = Duration(2 * time.Second)
	}
	// 注意：Required 与 CacheTTL 不在这里补默认值（显式写 false / 0 有意义），默认值由 Default() 提供
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogFormat == "" {
		c.LogFormat = "text"
	}
}

// Validate 校验配置。宁可启动失败，也不要带着错配置跑起来
// （典型如把流式路由配了总超时——那样的网关会在对话中途把 SSE 掐断）
func (c *Config) Validate() error {
	if len(c.Routes) == 0 {
		return fmt.Errorf("至少要配置一条路由（routes）")
	}
	if c.RateLimit.RPS < 0 {
		return fmt.Errorf("rate_limit.rps 不能为负数: %v", c.RateLimit.RPS)
	}
	if c.RateLimit.Burst < 0 {
		return fmt.Errorf("rate_limit.burst 不能为负数: %d", c.RateLimit.Burst)
	}
	if c.MaxConcurrentStreams < 0 {
		return fmt.Errorf("max_concurrent_streams 不能为负数: %d", c.MaxConcurrentStreams)
	}
	if c.Health.UpstreamURL != "" {
		u, err := url.Parse(c.Health.UpstreamURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("health.upstream_url %q 不是合法的 http(s) 地址（示例: http://127.0.0.1:8000/healthz）", c.Health.UpstreamURL)
		}
		if c.Health.Timeout < 0 {
			return fmt.Errorf("health.timeout 不能为负数")
		}
		if c.Health.CacheTTL < 0 {
			return fmt.Errorf("health.cache_ttl 不能为负数（0 表示每次真探）")
		}
	}

	seen := make(map[string]int, len(c.Routes))
	for i := range c.Routes {
		rt := &c.Routes[i]
		where := fmt.Sprintf("routes[%d]", i)

		if rt.Path == "" {
			return fmt.Errorf("%s: path 不能为空", where)
		}
		if !strings.HasPrefix(rt.Path, "/") {
			return fmt.Errorf("%s (%s): path 必须以 / 开头", where, rt.Path)
		}
		if prev, dup := seen[rt.Path]; dup {
			return fmt.Errorf("%s: path %q 与 routes[%d] 重复", where, rt.Path, prev)
		}
		seen[rt.Path] = i

		u, err := url.Parse(rt.Backend)
		if err != nil {
			return fmt.Errorf("%s (%s): backend %q 不是合法 URL，必须以 http:// 或 https:// 开头: %w",
				where, rt.Path, rt.Backend, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("%s (%s): backend %q 必须以 http:// 或 https:// 开头", where, rt.Path, rt.Backend)
		}
		if u.Host == "" {
			return fmt.Errorf("%s (%s): backend %q 缺少主机名", where, rt.Path, rt.Backend)
		}

		if rt.Timeout < 0 {
			return fmt.Errorf("%s (%s): timeout 不能为负数", where, rt.Path)
		}
		if rt.Streaming && rt.Timeout > 0 {
			return fmt.Errorf(
				"%s (%s): streaming 路由不能设置 timeout（当前 %s）——总超时会硬切断 SSE 流，"+
					"客户端表现为对话说到一半断线。流式路由请去掉 timeout，"+
					"用 max_concurrent_streams 限制并发、用 idle_timeout 回收僵死连接",
				where, rt.Path, rt.Timeout.Duration(),
			)
		}
	}
	return nil
}
