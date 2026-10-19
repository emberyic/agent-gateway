package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return path
}

func TestLoadParsesDurationsAndKeepsDefaults(t *testing.T) {
	path := writeConfig(t, `
listen: "127.0.0.1:9999"
rate_limit:
  rps: 12.5
  burst: 7
max_concurrent_streams: 4
backend_response_header_timeout: 5s
routes:
  - path: /chat/stream
    backend: http://127.0.0.1:8000
    streaming: true
  - path: /api
    backend: http://127.0.0.1:8081
    timeout: 90
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.RateLimit.RPS != 12.5 || cfg.RateLimit.Burst != 7 {
		t.Errorf("rate_limit = %+v", cfg.RateLimit)
	}
	if cfg.MaxConcurrentStreams != 4 {
		t.Errorf("max_concurrent_streams = %d", cfg.MaxConcurrentStreams)
	}
	if cfg.BackendResponseHeaderTimeout.Duration() != 5*time.Second {
		t.Errorf("backend_response_header_timeout = %v", cfg.BackendResponseHeaderTimeout.Duration())
	}
	// 配置里没写的字段应该落到默认值
	if cfg.ReadHeaderTimeout.Duration() != 10*time.Second {
		t.Errorf("read_header_timeout 应取默认值 10s，实际 %v", cfg.ReadHeaderTimeout.Duration())
	}
	if cfg.LogFormat != "text" {
		t.Errorf("log_format 应取默认值 text，实际 %q", cfg.LogFormat)
	}
	if !cfg.Routes[0].Streaming {
		t.Error("routes[0].streaming 应为 true")
	}
	// 纯数字按秒解释
	if cfg.Routes[1].Timeout.Duration() != 90*time.Second {
		t.Errorf("timeout: 90 应按 90 秒解释，实际 %v", cfg.Routes[1].Timeout.Duration())
	}
}

func TestValidateRejectsStreamingWithTimeout(t *testing.T) {
	// 最重要的一条校验：流式路由配总超时 = 对话说到一半被切断
	cfg := Default()
	cfg.Routes = []Route{{
		Path:      "/chat/stream",
		Backend:   "http://127.0.0.1:8000",
		Streaming: true,
		Timeout:   Duration(30 * time.Second),
	}}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("streaming 路由带 timeout 应当被拒绝")
	}
	if !strings.Contains(err.Error(), "streaming") {
		t.Fatalf("错误信息应点明 streaming 路由的问题，实际: %v", err)
	}
}

func TestValidateRejectsBadRoutes(t *testing.T) {
	cases := []struct {
		name   string
		routes []Route
		want   string
	}{
		{
			name:   "后端缺少 scheme",
			routes: []Route{{Path: "/api", Backend: "127.0.0.1:8081"}},
			want:   "http://",
		},
		{
			name:   "path 不以斜杠开头",
			routes: []Route{{Path: "api", Backend: "http://127.0.0.1:8081"}},
			want:   "以 / 开头",
		},
		{
			name: "path 重复",
			routes: []Route{
				{Path: "/api", Backend: "http://127.0.0.1:8081"},
				{Path: "/api", Backend: "http://127.0.0.1:8082"},
			},
			want: "重复",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Routes = tc.routes
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("应当校验失败（期望包含 %q）", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

func TestValidateRejectsEmptyRoutes(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err == nil {
		t.Fatal("没有任何路由时应当校验失败")
	}
}

func TestValidateChecksHealthUpstreamURL(t *testing.T) {
	cfg := Default()
	cfg.Routes = []Route{{Path: "/", Backend: "http://127.0.0.1:8081"}}
	if !cfg.Health.Required {
		t.Error("默认应当要求上游健康（required: true）")
	}

	cfg.Health.UpstreamURL = "127.0.0.1:8000/healthz" // 少了 scheme
	err := cfg.Validate()
	if err == nil {
		t.Fatal("health.upstream_url 不是合法 http(s) 地址时应当校验失败")
	}
	if !strings.Contains(err.Error(), "upstream_url") {
		t.Fatalf("错误信息应点明 upstream_url，实际: %v", err)
	}
}

func TestApplyDefaultsFillsBurstFromRPS(t *testing.T) {
	cfg := Default()
	cfg.RateLimit = RateLimit{RPS: 20} // burst 留空
	cfg.applyDefaults()
	if cfg.RateLimit.Burst != 20 {
		t.Fatalf("burst 应从容 rps 推导为 20，实际 %d", cfg.RateLimit.Burst)
	}
}

// 仓库里自带的配置文件必须都能加载 —— 否则改了 YAML 却没有任何测试覆盖，
// 只有真正启动网关时才会发现（docker 那份尤其容易漏）
func TestShippedConfigsAreValid(t *testing.T) {
	for _, name := range []string{"gateway.yml", "gateway.test.yml", "gateway.docker.yml"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(filepath.Join("..", "..", "configs", name))
			if err != nil {
				t.Fatalf("自带配置 %s 加载失败: %v", name, err)
			}
			if len(cfg.Routes) == 0 {
				t.Fatalf("%s 里没有任何路由", name)
			}
		})
	}
}
