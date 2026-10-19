"""FastAPI 外壳：SSE 对话 + 健康检查 + 指标 + 静态资源。"""

from __future__ import annotations

import os

# 必须在导入任何会拉起 huggingface_hub 的库之前设置：它在 import 时就把 HF_ENDPOINT
# 读成了模块常量，之后再改环境变量无效（顺序错了会卡在被墙的 huggingface.co 上重试）
os.environ.setdefault("HF_ENDPOINT", "https://hf-mirror.com")

import asyncio  # noqa: E402
import contextlib  # noqa: E402
import json  # noqa: E402
import logging  # noqa: E402
import time  # noqa: E402

import uvicorn  # noqa: E402
from fastapi import FastAPI, Request  # noqa: E402
from fastapi.responses import FileResponse, JSONResponse, Response, StreamingResponse  # noqa: E402
from fastapi.staticfiles import StaticFiles  # noqa: E402
from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver  # noqa: E402

from agent import build_agent  # noqa: E402
from health import check_llm_reachable, check_sqlite, run_checks  # noqa: E402
from history import visible_text  # noqa: E402
from knowledge import build_retriever  # noqa: E402
from metrics import METRICS  # noqa: E402
from models import classify_failure  # noqa: E402
from settings import Settings, load_settings  # noqa: E402

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
logger = logging.getLogger("agent.main")

SETTINGS: Settings = load_settings()

# SSE 响应头：禁用缓存与中间层缓冲（Go 网关也会再补一次，两侧都写才保险）
SSE_HEADERS = {
    "Cache-Control": "no-cache",
    "X-Accel-Buffering": "no",
}


def _sse_data(text: str) -> str:
    """把一段文本渲染成 SSE 帧。"""
    body = "\n".join(f"data: {line}" for line in text.split("\n"))
    return f"{body}\n\n"


def _should_flush(buffer: str, chunk: str, buffer_since: float, settings: Settings) -> bool:
    """攒批策略：断句符 / 长度 / 时间，任一满足就把缓冲区吐出去。"""
    if any(ch in settings.stream_flush_punct for ch in chunk):
        return True
    if len(buffer) >= settings.stream_flush_chars:
        return True
    return (time.monotonic() - buffer_since) >= settings.stream_flush_pause


@contextlib.asynccontextmanager
async def lifespan(app: FastAPI):
    settings = SETTINGS
    logger.info("启动配置: %s", json.dumps(settings.describe(), ensure_ascii=False))

    settings.checkpoint_path.parent.mkdir(parents=True, exist_ok=True)

    # 知识库：结构照搬原实现（内存向量库，启动时重建）
    retriever = build_retriever(
        knowledge_file=settings.knowledge_file,
        embedding_model=settings.embedding_model,
        top_k=settings.rag_top_k,
    )
    app.state.settings = settings
    app.state.retriever = retriever

    # 记忆：结构不变，仍是 AsyncSqliteSaver；它与 agent 共享 thread_id 空间
    async with AsyncSqliteSaver.from_conn_string(str(settings.checkpoint_path)) as memory:
        app.state.agent = build_agent(settings=settings, checkpointer=memory, retriever=retriever)
        logger.info("Agent 已挂载，等待请求")
        yield
    logger.info("应用关闭")


app = FastAPI(lifespan=lifespan)


@app.middleware("http")
async def _count_requests(request: Request, call_next):
    METRICS.inc_request()
    return await call_next(request)


# 前端页面与静态资源：只挂 assets/ —— 绝不能再挂项目根目录（那会暴露 .env 和记忆库）
if SETTINGS.assets_dir.is_dir():
    app.mount("/static", StaticFiles(directory=str(SETTINGS.assets_dir)), name="static")


@app.get("/")
def read_root():
    return FileResponse(SETTINGS.assets_dir / "index.html")


@app.get("/healthz")
async def healthz(request: Request):
    """并发探测各依赖并聚合：全好 → 200，任一坏 → 503（形状对齐 agent-go 的 health 包）。"""
    settings: Settings = request.app.state.settings
    deep = request.query_params.get("llm", "").lower() in ("1", "true", "yes")

    async def memory_ok() -> None:
        # sqlite3 是阻塞调用，丢到线程里跑，别卡住事件循环
        await asyncio.to_thread(check_sqlite, settings.checkpoint_path)

    async def knowledge_ok() -> None:
        if getattr(request.app.state, "retriever", None) is None:
            raise RuntimeError("知识库未就绪")

    async def llm_config_ok() -> None:
        if getattr(request.app.state, "agent", None) is None:
            raise RuntimeError("agent 未装配")
        if not settings.api_key:
            raise RuntimeError("缺少主模型 API Key")

    checks = {"memory": memory_ok, "knowledge": knowledge_ok, "llm_config": llm_config_ok}
    if deep:
        checks["llm_reachable"] = lambda: check_llm_reachable(settings)

    ok, body = await run_checks(checks)
    return JSONResponse(
        {
            "status": "ok" if ok else "unhealthy",
            "checks": body,
            "deep": deep,
            "config": settings.describe(),
        },
        status_code=200 if ok else 503,
    )


@app.get("/metrics")
async def metrics_endpoint() -> Response:
    return Response(content=METRICS.render(), media_type="text/plain; version=0.0.4; charset=utf-8")


@app.get("/chat/stream")
async def chat_stream(request: Request, question: str, session_id: str = "default_user"):
    """SSE 对话。帧格式保持原样（``data: <文本>``），前端 index.html 无需改动。"""
    agent = request.app.state.agent
    settings: Settings = request.app.state.settings

    async def event_stream():
        started = time.monotonic()
        METRICS.stream_started()

        queue: asyncio.Queue = asyncio.Queue(maxsize=128)
        sentinel = object()

        async def pump() -> None:
            """把 agent 的流搬进队列。"""
            try:
                async for message, metadata in agent.astream(
                    {"messages": [{"role": "user", "content": question}]},
                    config={"configurable": {"thread_id": session_id}},
                    stream_mode="messages",
                ):
                    await queue.put((message, metadata))
            except BaseException as exc:  # noqa: BLE001 — 含 CancelledError，都要让消费端退出
                with contextlib.suppress(Exception):
                    queue.put_nowait(exc)
            finally:
                with contextlib.suppress(Exception):
                    queue.put_nowait(sentinel)

        task = asyncio.create_task(pump())
        buffer = ""
        buffer_since = 0.0

        try:
            while True:
                try:
                    item = await asyncio.wait_for(queue.get(), timeout=settings.heartbeat_seconds)
                except asyncio.TimeoutError:
                    # 空闲：先把攒着的文本吐出去（时间维度的兜底），再发心跳保活
                    if buffer.strip():
                        yield _sse_data(buffer.strip())
                        METRICS.inc_frame()
                        buffer, buffer_since = "", 0.0
                    METRICS.inc_heartbeat()
                    yield ": ping\n\n"
                    continue

                if item is sentinel:
                    break
                if isinstance(item, BaseException):
                    raise item

                message, _metadata = item
                # 工具消息不进正文（原实现就是这么过滤的），只用来计数
                if getattr(message, "type", "") == "tool":
                    METRICS.tool_result(str(getattr(message, "name", "") or ""))
                    continue

                text = visible_text(message)
                if not text:
                    continue
                if not buffer:
                    buffer_since = time.monotonic()
                buffer += text
                if _should_flush(buffer, text, buffer_since, settings):
                    if buffer.strip():
                        yield _sse_data(buffer.strip())
                        METRICS.inc_frame()
                    buffer, buffer_since = "", 0.0

            if buffer.strip():
                yield _sse_data(buffer.strip())
                METRICS.inc_frame()
            yield 'event: done\ndata: {"messageType":"done"}\n\n'
            METRICS.observe_run(time.monotonic() - started)

        except asyncio.CancelledError:
            # 客户端断开 → Starlette 取消这个生成器。顺手取消上游 run，
            # 否则 LLM 会继续生成（继续烧 token）。这是原实现漏掉的一步
            logger.info("客户端断开，取消上游 run: session_id=%s", session_id)
            raise

        except Exception as exc:  # noqa: BLE001 — 兜底：任何失败都要给客户端一个明确收尾
            code = classify_failure(exc)
            METRICS.error(code)
            logger.exception("对话失败: session_id=%s", session_id)
            payload = {"code": code, "message": str(exc)[:300]}
            yield f"event: error\ndata: {json.dumps(payload, ensure_ascii=False)}\n\n"
            yield 'event: done\ndata: {"messageType":"done"}\n\n'

        finally:
            METRICS.stream_finished()
            task.cancel()
            with contextlib.suppress(BaseException):
                await task

    return StreamingResponse(event_stream(), media_type="text/event-stream", headers=SSE_HEADERS)


if __name__ == "__main__":
    uvicorn.run(app, host=SETTINGS.host, port=SETTINGS.port)
