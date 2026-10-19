// test_backend.go 是一个本地测试后端，用来在没有 Agent 后端时验证网关：
// go run test_backend.go，再 go run ./cmd/gateway -config configs/gateway.test.yml
package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

func main() {
	mux := http.NewServeMux()

	// 给 wrk / ab 压测用
	mux.HandleFunc("/api/test", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond) // 模拟后端处理耗时
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Backend", "test-backend")
		fmt.Fprintf(w, "后端响应: %s | 时间: %s\n", r.URL.Path, time.Now().Format("15:04:05.000"))
	})

	// 如果网关有缓冲，客户端会一次性收到全部帧，而不是每 300ms 收到一帧
	mux.HandleFunc("/chat/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "当前 ResponseWriter 不支持 flush", http.StatusInternalServerError)
			return
		}
		question := r.URL.Query().Get("question")
		sessionID := r.URL.Query().Get("session_id")

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		// EventSource 只能发 GET、不能带自定义请求头，参数只能走 query
		// 第一帧就把 query 回显出来：网关有没有丢 query，一眼可见
		fmt.Fprintf(w, "data: 收到 question=%q session_id=%q\n\n", question, sessionID)
		flusher.Flush()

		frames := 5
		if v := r.URL.Query().Get("frames"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				frames = n
			}
		}
		for i := 1; i <= frames; i++ {
			select {
			case <-r.Context().Done(): // 客户端断开
				log.Printf("客户端断开: session_id=%s 第 %d 帧", sessionID, i)
				return
			case <-time.After(300 * time.Millisecond):
			}
			fmt.Fprintf(w, "data: 第 %d/%d 帧\n\n", i, frames)
			flusher.Flush()
		}

		// 前端据此区分「正常说完」和「断网了」
		fmt.Fprint(w, "event: done\ndata: {\"messageType\":\"done\"}\n\n")
		flusher.Flush()
	})

	// 每帧间隔 1 秒，用来验证 max_concurrent_streams
	mux.HandleFunc("/chat/slow", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "当前 ResponseWriter 不支持 flush", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		for i := 1; i <= 10; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
			}
			fmt.Fprintf(w, "data: slow %d/10\n\n", i)
			flusher.Flush()
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "test-backend v2\n接口: /api/test、/chat/stream?question=..&session_id=..、/chat/slow、/healthz\n")
	})

	// 模拟 Agent 后端的 /healthz，用来验证网关的健康聚合
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprint(w, `{"status":"ok","checks":{"test_backend":"ok"}}`)
	})

	const addr = ":8081"
	log.Printf("测试后端运行在 http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("测试后端启动失败: %v", err)
	}
}
