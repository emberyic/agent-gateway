# agent-gateway — AI 网关 + Agent 后端

- **Go 网关**（本目录根）：统一入口、路径路由、IP 限流、流式（SSE）并发准入、
  零缓冲流式透传、健康聚合、指标与优雅退出；
- **Python Agent 后端**（`agent/`）：FastAPI + LangChain + LangGraph，
  多工具调度、本地知识库检索、SQLite 持久化多轮记忆、历史裁剪、模型降级、SSE 流式输出。

```
浏览器 / 客户端
      │  HTTP + SSE
      ▼
 agent-gateway (Go, :8080)        ← 边缘层：路由 / 限流 / 并发闸 / 健康聚合 / 指标
      │  HTTP + SSE
      ▼
 agent       (Python, :8000)      ← Agent 侧：图 / 工具 / RAG / 记忆 / 裁剪 / 降级
```

分工：**网关只做边缘，不做 Agent 编排**。它不解析、不缓冲、不改写流式响应体，
只负责「把请求安全地送到后端，并在后端扛不住之前把多余流量挡在外面」。

---

## 1. 快速开始

### 1.1 只用 Go 本地验证（不需要 Agent 后端）

```bash
# 终端 1：本地测试后端（普通接口 + SSE + 慢速 SSE + /healthz）
go run test_backend.go

# 终端 2：网关（小限流 + 并发闸，方便观察 429）
go run ./cmd/gateway -config configs/gateway.test.yml

# 终端 3：一键验证（SSE 逐帧时间戳 / 限流 / 并发闸）
bash scripts/benchmark.sh
```

手动看 SSE 是否真的逐帧下发：

```bash
curl -N 'http://localhost:8080/chat/stream?question=hello&session_id=u1&frames=3'
# data: 收到 question="hello" session_id="u1"   ← query 原样到达后端
# （约 0.3 秒后）
# data: 第 1/3 帧
# ...
```

### 1.2 接 Agent 后端（`agent/`）

先准备 Agent 后端：

```bash
cd agent
cp .env.example .env          # 填 DEEPSEEK_API_KEY（备用模型、和风 Key 可选）
python -m pip install -r requirements.txt

python main.py                # 或 uvicorn main:app --host 127.0.0.1 --port 8000
python selftest.py            # 离线自检：不需要 Key、不联网、不起服务
```

> **必须在 `agent/` 目录下启动**（或把 IDE 的工作目录设成它）：模块之间是平铺导入
> （`import settings`），用 `uvicorn agent.main:app` 从上层目录是起不来的。

再启动网关：

```bash
go run ./cmd/gateway                # :8080
# 默认配置就把 /chat/stream、/static、/agent、/ 指向 :8000
# 打开 http://localhost:8080 —— index.html 里的 /chat/stream 是相对路径，
# 所以从网关访问时，前端一行都不用改。
```

### 1.3 Docker

```bash
docker compose up -d --build
# 配置用 configs/gateway.docker.yml；容器经 host.docker.internal 访问宿主机上的
# Agent 后端（:8000）。参数可用环境变量覆盖，见 docker-compose.yml 注释。
```

---

## 2. 网关配置

配置文件默认 `configs/gateway.yml`（可用 `-config` 或 `GATEWAY_CONFIG` 指定）：

| 字段 | 说明 | 默认 |
|---|---|---|
| `listen` | 监听地址 | `0.0.0.0:8080` |
| `trust_proxy` | 是否采信 `X-Forwarded-For` 识别客户端 IP。**只有确部署在可信反代之后才能开**，直连暴露时打开等于让人伪造 XFF 换限流桶 | `false` |
| `rate_limit.rps` / `.burst` | 按客户端 IP 的令牌桶（每秒补 rps，最多攒 burst）。`rps: 0` 关闭 | `50 / 100` |
| `max_concurrent_streams` | 同时在跑的**流式**请求上限，超了 429。`0` 表示不限 | `64` |
| `backend_response_header_timeout` | 等后端开始回响应头的上限（**不含**响应体传输时间） | `60s` |
| `health.upstream_url` | `/readyz` 里要探测的上游健康检查地址（如 `http://127.0.0.1:8000/healthz`）。留空则 `/readyz` 也只报网关自身 | 空 |
| `health.timeout` | 单次探测上游的超时 | `2s` |
| `health.cache_ttl` | 探测结果缓存时长，避免高频就绪探针把压力放大到后端。`0` = 不缓存 | `1s` |
| `health.required` | 上游不健康时，网关 `/readyz` 是否返回 503 | `true` |
| `read_header_timeout` / `idle_timeout` / `shutdown_timeout` | 读请求头 / 空闲连接 / 优雅关闭等待 | `10s / 120s / 15s` |
| `log_level` / `log_format` | `debug|info|warn|error` / `text|json` | `info / text` |
| `routes[].path` | 匹配前缀，`/` 为兜底 | 必填 |
| `routes[].backend` | 后端地址，必须带 `http://` 或 `https://` | 必填 |
| `routes[].strip_prefix` | 转发时剥掉 `path` 前缀（`/agent/healthz` → 后端收到 `/healthz`） | `false` |
| `routes[].streaming` | 标记为 SSE：**立即 flush + 计入并发闸 + 禁止配 timeout** | `false` |
| `routes[].timeout` | 该路由总超时，`0` 不限。**流式路由配置它会直接启动失败** | `0` |

启动时会校验：至少一条路由、path 以 `/` 开头且不重复、backend 是合法 http(s) URL、
`health.upstream_url` 合法。**宁可启动失败，也不带着错配置跑** —— 尤其是「给流式路由配总超时」，
那会让对话说到一半断线。

环境变量覆盖（容器里调参不用改文件、不用重构建）：

```
GATEWAY_CONFIG / GATEWAY_LISTEN / GATEWAY_LOG_LEVEL / GATEWAY_LOG_FORMAT
GATEWAY_RATE_LIMIT_RPS / GATEWAY_RATE_LIMIT_BURST / GATEWAY_MAX_STREAMS / GATEWAY_TRUST_PROXY
```

路由按**最长前缀优先**匹配（与配置顺序无关），所以 `"/"` 兜底可以放最前面：

```
/chat/stream  → http://127.0.0.1:8000   [sse]          ← 流式，零缓冲
/static       → http://127.0.0.1:8000                  ← 前端静态资源
/agent        → http://127.0.0.1:8000   [strip_prefix] ← 运维：/agent/healthz、/agent/metrics
/api          → http://127.0.0.1:8081   [http, 30s]
/             → http://127.0.0.1:8000   [http, 60s]    ← 兜底
```

> 新增流式接口时**必须单独列一条 `streaming: true` 的路由**。否则它会被兜底路由的
> `timeout` 接管，长连接在传输中途被切断。

---

## 3. 流式（SSE）转发：三条保证

1. **query 原样保留。** `EventSource` 只能发 GET、不能带自定义请求头，所以对话接口把参数
   放在 query 上（`/chat/stream?question=..&session_id=..`）。转发用
   `ProxyRequest.SetURL`（只改 scheme/host/path），RawQuery 原样透传 —— 网关一旦丢掉 query，
   后端就永远收不到问题。
2. **每一帧立即下发，网关零缓冲。** 后端已经按标点/字数把输出切好并逐帧 flush，
   网关再攒一批发出去，前端的流式效果就全没了。流式路由用 `FlushInterval = -1`；
   即使某条路由忘了标 `streaming`，Go 的 `ReverseProxy` 也会对 `text/event-stream`
   自动立即 flush（双保险）。
3. **不碰响应体。** 网关不解析、不重组 SSE 帧，只在流式响应上补 `Cache-Control: no-cache`
   和 `X-Accel-Buffering: no`（前面还挂着 nginx 时，少了后者 nginx 会攒批）。

配套的两个细节，都是「包装 ResponseWriter」时最容易踩的坑：

- 访问日志用的 `responseRecorder` 必须实现 `Flush()` 和 `Unwrap()`。少了前者，
  `ReverseProxy` 拿不到 `http.Flusher` 就无法立即 flush；少了后者，
  `http.ResponseController` 找不到底层 writer。**这两个方法就是 SSE 的生命线。**
- `withRecover` 必须放行 `http.ErrAbortHandler`。这是标准库的哨兵 panic
  （客户端中途断开、响应体拷贝失败时 `ReverseProxy` 会抛它），`net/http` 认得它并会
  静默关闭连接；一旦被自己的 recover 吞掉，网关会往**已经开始的 SSE 流里**写一个 500，
  把响应彻底弄脏。

另外刻意**不设** `http.Server` 的 `WriteTimeout` / `ReadTimeout`：

- `WriteTimeout` 覆盖「读完请求头 → 整个响应写完」，设上之后任何超过它的 SSE 流都会被硬切断；
- 长连接的保护交给路由级 `timeout`（仅非流式）、`max_concurrent_streams`、
  `idle_timeout` 和客户端断开时的 context 取消。

---

## 4. Agent 侧

### 4.1 上下文有界：滑动窗口裁剪

`create_agent` 的 messages 通道会一路累积，长会话必然 token 爆炸。
`HistoryTrimMiddleware` 在**每次模型调用前**把历史投影成有界视图：

- 只作用于这一次调用（`ModelRequest.override(messages=...)`），**checkpoint 里一条不删**
  —— 想删得构造 `RemoveMessage`，那才是动了记忆结构；
- 按 Human 边界切「轮」，从新到旧保留最近 `AGENT_HISTORY_MAX_ROUNDS` 轮（默认 8）；
- **绝不拆散 `tool_calls` ↔ `tool_result` 配对**：配对天然落在同一轮里，剪断会让 provider 400；
- 字符预算是第二道闸（`AGENT_HISTORY_MAX_CHARS`，默认 24000）：超了就从最旧的保留轮丢，
  最后一轮永远保留；
- 首条 Human 被裁掉时，补一条**标注「仅供背景参考」的 SystemMessage**。

最后一条的动机：把首条 Human **原文**塞回窗口，等于把用户最早的**指令**变成紧邻本轮问题的
**新**指令。反例——会话第一条是「只回复"好"」，裁剪后重放，之后问「用三点列出 Go 的优点」
模型会回「好」。`agent/selftest.py` 里有这条回归测试。

> 被挤出的轮次目前直接丢弃。另一种做法是折叠成一条确定性摘要（保留脉络、不重放原文），
> 见 `agent-go/cognition/cognition/graphs/history.py`。

### 4.2 超时与降级：三层

```
SDK 层    timeout 限制单次 HTTP 请求（含一次流式读）；max_retries 处理 429/5xx
中间件层  主模型彻底失败 → 用备用模型重试同一次调用（不动 state、不动 checkpoint）
应用层    两个都失败 → 发 event: error + event: done，绝不静默返回空答案
```

降级三项都填才启用：

```env
AGENT_FALLBACK_MODEL=deepseek-chat          # 可以是任意 OpenAI 兼容端点
AGENT_FALLBACK_BASE_URL=https://api.deepseek.com
AGENT_FALLBACK_API_KEY=sk-...
```

两条边界：

1. **降级只保证「首个 token 之前」干净。** 流到一半才失败时，已经吐出去的字收不回来，
   客户端应把 `event: error` 当终止信号（这也是需要这个事件的原因）。
2. **SSE 一旦开始发帧，HTTP 状态码就固定是 200**，失败只能靠事件表达。
   网关侧看到的是 200，不能据此判断成功 —— 要看 `event: done` 有没有来。

### 4.3 流式输出：攒批 + 心跳 + 断连取消

攒批条件是三者任一满足：

| 条件 | 配置 | 作用 |
|---|---|---|
| 断句符 | `AGENT_STREAM_FLUSH_PUNCT`（默认 `。！？!?；;`，**不含换行**） | 一句话说完就发。markdown 换行太频繁，带上它等于每个 chunk 都 flush |
| 长度 | `AGENT_STREAM_FLUSH_CHARS`（默认 200） | 没有标点时防止碎片化 |
| 时间 | `AGENT_STREAM_FLUSH_PAUSE_SECONDS`（默认 0.15） | 模型连着吐几百字都没标点时，仍然按时下发 |

另外两件事：

- **空闲心跳**（`AGENT_HEARTBEAT_SECONDS`，默认 10s）：发 SSE 注释帧 `: ping`，
  防止长思考期间被中间层按空闲超时切断；
- **客户端断开时取消上游 run**：Starlette 取消生成器后顺手取消 agent 的流，
  否则 LLM 会继续生成、继续烧 token。

SSE 帧的 `data:` **必须逐行加前缀**：data 字段里出现裸换行会破坏帧结构
（模型输出的 markdown 列表全是换行），浏览器会把第二行当成「没有冒号的字段行」丢掉，
表现为「回答只显示第一行」。多行 data 由浏览器按规范用 `\n` 拼回，前端无需特殊处理。

---

## 5. Agent 配置

全部通过环境变量（或 `agent/.env`）配置，见 [`agent/.env.example`](agent/.env.example)。
常用的几项：

| 变量 | 默认 | 说明 |
|---|---|---|
| `DEEPSEEK_API_KEY` | 无 | 主模型 Key，缺失时启动直接失败 |
| `AGENT_MODEL` / `AGENT_BASE_URL` | `deepseek-chat` / `https://api.deepseek.com` | 主模型 |
| `AGENT_LLM_TIMEOUT_SECONDS` / `AGENT_LLM_MAX_RETRIES` | `60` / `1` | 单次请求超时与 SDK 重试 |
| `AGENT_FALLBACK_MODEL` / `_BASE_URL` / `_API_KEY` | 空 | 备用模型（三项齐才启用降级） |
| `AGENT_HISTORY_MAX_ROUNDS` / `_MAX_CHARS` | `8` / `24000` | 滑动窗口预算 |
| `AGENT_HISTORY_KEEP_FIRST_HUMAN` | `true` | 首条 Human 被裁掉时补背景说明 |
| `AGENT_HISTORY_REPAIR_TOOL_CALLS` | `true` | 入模型前修复悬空/损坏的 tool_calls |
| `AGENT_STREAM_FLUSH_*` / `AGENT_HEARTBEAT_SECONDS` | 见 4.3 | 攒批与心跳 |
| `AGENT_KNOWLEDGE_FILE` / `AGENT_RAG_TOP_K` | `knowledge/my_knowledge.txt` / `3` | 知识库文件与检索条数 |
| `AGENT_CHECKPOINT_PATH` | `data/checkpoints.db` | 记忆库路径 |
| `AGENT_HOST` / `AGENT_PORT` | `127.0.0.1` / `8000` | 监听地址 |

`QWEATHER_API_KEY` 未配置时天气工具会返回一句提示，不影响其他功能。

---

## 6. 高并发保护：两层

| 层 | 管什么 | 超限行为 |
|---|---|---|
| IP 限流（令牌桶） | 「每秒来几个请求」，防刷、防爬 | `429` + `Retry-After` + `X-RateLimit-*` |
| 流式并发闸 | 「同时挂着几条流」，保护后端与 LLM 上游 | `429` + `Retry-After: 1`，错误码 `too_many_streams` |

为什么光有 QPS 限流不够：一个 SSE 请求能挂几分钟，QPS 只看入口速率，对「同时挂着多少」
毫无办法 —— 100 个用户同时开始对话就是 100 路并发 LLM 调用。这就是
`max_concurrent_streams` 存在的原因，也是把准入放在「写任何响应头之前」的原因
（否则没法干净地回 429）。

错误响应统一为 `{"code": "...", "message": "..."}`：
`rate_limited` / `too_many_streams` / `route_not_found` / `bad_gateway` /
`backend_timeout` / `internal_error`。

客户端 IP 的取法：默认只信 `RemoteAddr`；开启 `trust_proxy` 后取 `X-Forwarded-For` 的第一段，
并把整条链透传给后端（`X-Forwarded-For`）+ 真实客户端写入 `X-Real-IP`。
**默认不信任 XFF** 是刻意的：否则任何人都能伪造 XFF 给每个请求换一个新限流桶。

---

## 7. 端点

### 7.1 网关自身

| 端点 | 用途 |
|---|---|
| `GET /healthz` | **存活探针**：只报网关自身（时间、路由表、在途请求数、流式水位），**永远 200，不探上游**。存活探针一旦依赖下游，后端故障就会被编排判成"网关也坏了"而重启网关——那时重启毫无帮助。**不参与限流**（否则健康检查会被自己打成 429） |
| `GET /readyz` | **就绪探针**：在 `/healthz` 的基础上聚合上游（Agent 后端），带上 `upstream`（ok / status / 延迟 / 是否命中缓存 / 后端自报的原因）。上游不健康且 `health.required: true` 时返回 **503**。不参与限流；探测结果按 `health.cache_ttl` 缓存（默认 1s，防止高频探针放大到后端） |
| `GET /metrics` | Prometheus 文本格式指标（零依赖实现） |
| `GET /agent/healthz`、`/agent/metrics` | 转发到后端自己的同名端点（配置里的 `/agent` 路由 + `strip_prefix`），一个入口看两面 |

关注的三个数字正对应三个风险点：

```
gateway_rate_limited_total      QPS 限流挡掉了多少
gateway_stream_rejected_total   流式并发满了拒绝了多少（后端保护是否生效）
gateway_streams_in_flight       此刻有多少条长连接挂着（容量水位）
```

每个响应都带 `X-Request-Id`（客户端可自带，否则网关生成），日志里用它串联。
访问日志**只记 path 不记 query** —— 用户的问题就在 query 里，原样落盘等于记录用户输入。

### 7.2 Agent 后端

| 端点 | 说明 |
|---|---|
| `GET /chat/stream?question=..&session_id=..` | SSE 对话。帧格式 `data: <文本>`，收尾 `event: done`，失败 `event: error`，空闲 `: ping` |
| `GET /healthz` | 并发探测 `memory`（SQLite 可读）/ `knowledge`（检索器就绪）/ `llm_config`（Key + agent 装配）；全好 200，任一坏 503。`?llm=1` 追加深度探测（`GET {base_url}/models`，不耗 token） |
| `GET /metrics` | Prometheus 文本（`agent_*`） |
| `GET /` | `agent/assets/index.html` |
| `GET /static/*` | 只挂 `agent/assets/` 目录 |

**`/static` 只能挂 `assets/`**：静态资源目录必须是一个只放前端文件的子目录。若把它设成
项目根目录（`StaticFiles(directory=".")`），`.env`（含 API Key）和 `data/checkpoints.db`
（全部对话记忆）都会变成可下载的静态文件：

```
GET /static/.env           → 200，含 API_KEY
GET /static/checkpoints.db → 200，全部对话记忆
```

---

## 8. 与 Agent 后端的契约

- `GET /chat/stream?question=..&session_id=..` 这个 **GET + query** 形态（`EventSource`
  不能带请求头，参数只能走 query）；
- SSE 帧格式 `data: <文本>`：收尾 `event: done`，失败 `event: error`，空闲 `: ping`。
  网关原样透传、不加自己的帧；
- 后端端口 `:8000`、首页 `/`、静态资源 `/static` —— 默认配置直接对齐，
  从网关访问时前端 `index.html` 不需要任何改动。

### 8.1 前端会话 ID

`index.html` 不再写死 `session_id`，改成**每个浏览器一个稳定的 ID**（它就是后端的
`thread_id`，所以换 ID 等于换一段记忆）：

| 时机 | 行为 |
|---|---|
| 首次访问 | `crypto.randomUUID()` 生成，存进 `localStorage["agent_session_id"]` |
| 之后每次请求 | 从 `localStorage` 读出来，拼进 `/chat/stream?question=..&session_id=..` |
| 刷新页面 | 沿用同一个 ID，续上同一段记忆 |
| 清浏览器缓存 / 换浏览器 / 无痕窗口 | 生成新 ID = 一段新记忆（跨设备续聊需要登录态） |

`crypto.randomUUID()` 只在安全上下文（`https` 或 `localhost`）可用，所以有降级链：
`randomUUID` → `getRandomValues`（拼成 32 位十六进制）→ 时间戳 + 随机数。`localStorage`
不可用时（隐私模式会抛异常）退化成「只在本次页面内有效」的内存 ID，避免每句话都变成新会话。

两点要清楚：这解决的是**串台**，不是鉴权 —— 客户端仍可自选 `session_id`，真正的隔离需要
登录态或服务端签发；换浏览器/换设备就会丢上下文，这是 localStorage 方案的天然边界。

---

## 9. 测试与验证

```bash
go test ./...                  # Go：18 个测试函数（含子测试 24 个用例）
go vet ./...
cd agent && python selftest.py # Python：17 项离线自检（不需要 Key、不联网）
```

覆盖的关键行为（Go 侧在 `pkg/gateway/gateway_test.go`、`pkg/config/config_test.go`；
Python 侧在 `agent/selftest.py`）：

- query / path 原样转发，`strip_prefix` 正确，最长前缀优先（与配置顺序无关）；
- 默认不采信客户端伪造的 XFF；开启 `trust_proxy` 后 `X-Real-IP` 与 XFF 链正确；
- **SSE 逐帧下发**：后端 flush 第一帧后（还没写第二帧），客户端必须已经能读到第一帧
  —— 这一条直接把「网关缓冲」钉死成测试失败；
- 流式并发闸满载回 `429 too_many_streams`，释放后可继续建立新流；
- IP 限流回 `429 rate_limited` + `Retry-After`；
- `ResponseHeaderTimeout` 只作用于响应头：等不到头回 `504`，而持续 1.2 秒的响应体
  必须完整收到（总超时会切断长连接的回归测试）；
- 未匹配路由回 404 JSON；`/healthz`、`/metrics` 不被路由和限流抢走；
- 存活/就绪分离：上游挂掉时 `/healthz` 仍 200（存活探针不依赖下游），`/readyz` 才 503 + `degraded`
  （`required: false` 时只标注不改状态码）；就绪探测在 TTL 内命中缓存、后端只被探一次；
- 访问日志带请求 ID 且不记录 query（用户的问题就在 query 里）；
- 仓库自带的三个配置文件都必须能加载；
- Python 侧：滑动窗口（含 tool 配对不变量、字符预算、锚点语义）、工具调用修复、
  指标输出、健康聚合与超时、SSE 多行帧、攒批三条件。

`scripts/benchmark.sh` 做端到端验证（带每帧到达时间戳，肉眼可判断有没有被缓冲）。

> 未覆盖：`go test -race` 需要 cgo（本机没有 C 编译器），
> 在有 gcc 的环境补跑 `CGO_ENABLED=1 go test -race ./pkg/...`。

---

## 10. 目录结构

```
agent-gateway/
├── cmd/gateway/main.go        入口：读配置 → 装配 → http.Server（明确不设 WriteTimeout）→ 优雅退出
├── pkg/config/config.go       配置加载/默认值/校验（含 Duration 的 "30s" 解析）
├── pkg/gateway/
│   ├── engine.go              转发引擎：共享 Transport 连接池 + BufferPool 内存池 + 每路由一个 ReverseProxy
│   ├── gateway.go             路由装配（最长前缀）、/healthz 存活 + /readyz 就绪聚合、/metrics、错误体与 writeJSON
│   ├── middleware.go          IP 限流、流式并发闸、请求 ID、recover、访问日志、responseRecorder
│   ├── health.go              上游（Agent 后端）健康探测
│   ├── metrics.go             零依赖 Prometheus 文本指标
│   └── gateway_test.go        转发/流式/限流/并发闸/超时语义/健康聚合的测试
├── agent/                     Python Agent 后端
│   ├── main.py                FastAPI 外壳：SSE 对话、/healthz、/metrics、静态资源、攒批策略
│   ├── agent.py               装配：工具集 + 模型降级 + 历史裁剪中间件 + checkpointer
│   ├── history.py             滑动窗口裁剪 + 工具调用配对修复（纯函数）
│   ├── models.py              模型构建、超时、主备降级中间件、失败原因归类
│   ├── knowledge.py           本地知识库 → search_knowledge 工具
│   ├── tools.py               时间 / 天气工具
│   ├── health.py              并发探测 + 聚合（/healthz 的实现）
│   ├── metrics.py             零依赖 Prometheus 文本指标
│   ├── settings.py            环境变量 → 只读 Settings
│   ├── selftest.py            离线自检（17 项）
│   ├── assets/index.html      前端页面（只挂这一个目录，不暴露项目根目录）
│   ├── knowledge/             本地知识库文档
│   ├── data/                  运行时数据（SQLite 记忆库，已 gitignore）
│   └── requirements.txt / .env.example
├── configs/
│   ├── gateway.yml            默认：接 Agent 后端（:8000）
│   ├── gateway.test.yml       本地：接 test_backend.go（:8081），小限流小并发便于观察 429
│   └── gateway.docker.yml     compose：后端地址换成容器网络名 / host.docker.internal
├── test_backend.go            本地测试后端（/api/test、/chat/stream、/chat/slow、/healthz）
├── scripts/benchmark.sh       端到端验证脚本
├── Dockerfile / docker-compose.yml
└── README.md
```
