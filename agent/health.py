"""健康检查：并发探测 + 聚合（形状照搬 agent-go 的 control-plane/internal/health）。"""

from __future__ import annotations

import asyncio
import logging
import sqlite3
from typing import Awaitable, Callable

import httpx

from settings import Settings

logger = logging.getLogger("agent.health")

Check = Callable[[], Awaitable[None]]

DEFAULT_CHECK_TIMEOUT = 2.0


def check_sqlite(path) -> None:
    """SQLite 记忆库可读（同步实现，调用方用 to_thread 包起来）。"""
    # 只读打开：探活不该创建文件
    uri = "file:" + str(path).replace("\\", "/") + "?mode=ro"
    conn = sqlite3.connect(uri, uri=True, timeout=DEFAULT_CHECK_TIMEOUT)
    try:
        conn.execute("SELECT 1")
    finally:
        conn.close()


async def check_llm_reachable(settings: Settings) -> None:
    """上游 LLM 可达：GET {base_url}/models，不消耗 token。"""
    url = f"{settings.base_url.rstrip('/')}/models"
    headers = {"Authorization": f"Bearer {settings.api_key}"}
    async with httpx.AsyncClient(trust_env=False, timeout=3.0) as client:
        response = await client.get(url, headers=headers)
    if response.status_code >= 400:
        raise RuntimeError(f"上游返回 {response.status_code}")


async def _run_check(name: str, check: Check, timeout: float) -> tuple[str, str | None]:
    try:
        await asyncio.wait_for(check(), timeout=timeout)
        return name, None
    except asyncio.TimeoutError:
        return name, f"超时（>{timeout:g}s）"
    except Exception as exc:  # noqa: BLE001 — 探测失败本身就是结论，不是异常
        logger.warning("健康检查 %s 失败: %s", name, exc)
        return name, f"{type(exc).__name__}: {exc}"


async def run_checks(
    checks: dict[str, Check],
    *,
    timeout: float = DEFAULT_CHECK_TIMEOUT,
) -> tuple[bool, dict[str, str]]:
    """并发跑完所有 check，聚合成 (是否健康, {name: "ok"|失败原因})。"""
    if not checks:
        return True, {}

    results = await asyncio.gather(*(_run_check(name, check, timeout) for name, check in checks.items()))
    body: dict[str, str] = {}
    ok = True
    for name, error in results:
        if error is None:
            body[name] = "ok"
        else:
            ok = False
            body[name] = error
    return ok, body
