"""模型构建 + 超时 + 主备降级。"""

from __future__ import annotations

import asyncio
import logging

from langchain.agents.middleware import AgentMiddleware, ModelRequest
from langchain.chat_models import init_chat_model
from langchain_core.language_models.chat_models import BaseChatModel

from metrics import METRICS
from settings import Settings

logger = logging.getLogger("agent.models")


def classify_failure(exc: BaseException) -> str:
    """把异常归成一个稳定的原因标签（用于指标与日志，不用于控制流）。"""
    if isinstance(exc, (asyncio.TimeoutError, TimeoutError)):
        return "timeout"

    text = f"{type(exc).__name__}: {exc}".lower()
    if "timeout" in text or "timed out" in text:
        return "timeout"
    if "rate limit" in text or "429" in text:
        return "rate_limit"
    if "api key" in text or "unauthorized" in text or "authentication" in text or "401" in text:
        return "auth"
    if "connection" in text or "connect" in text or "network" in text or "dns" in text:
        return "connection"
    if "500" in text or "502" in text or "503" in text or "server error" in text:
        return "server_error"
    return type(exc).__name__


def build_chat_model(
    *,
    model: str,
    base_url: str,
    api_key: str,
    timeout: float,
    max_retries: int,
    tag: str,
) -> BaseChatModel:
    """构建一个 OpenAI 兼容的 chat 模型（DeepSeek 走的就是这条）。"""
    if not api_key:
        raise ValueError(f"{tag} 缺少 API Key（检查 .env / 环境变量）")
    logger.info("构建模型 %s: model=%s base_url=%s timeout=%ss", tag, model, base_url, timeout)
    return init_chat_model(
        f"openai:{model}",
        base_url=base_url,
        api_key=api_key,
        timeout=timeout,
        max_retries=max_retries,
    )


def build_primary(settings: Settings) -> BaseChatModel:
    """主模型。缺 Key 时直接抛错 —— 启动即失败，好过运行时才发现。"""
    return build_chat_model(
        model=settings.model,
        base_url=settings.base_url,
        api_key=settings.api_key,
        timeout=settings.llm_timeout,
        max_retries=settings.llm_max_retries,
        tag="主模型",
    )


def build_fallback(settings: Settings) -> BaseChatModel | None:
    """备用模型；没配齐就返回 None（此时不启用降级）。"""
    if not settings.fallback_enabled:
        return None
    return build_chat_model(
        model=settings.fallback_model,
        base_url=settings.fallback_base_url,
        api_key=settings.fallback_api_key,
        timeout=settings.fallback_timeout,
        max_retries=settings.llm_max_retries,
        tag="备用模型",
    )


class FallbackModelMiddleware(AgentMiddleware):
    """主模型调用失败时，用备用模型重试同一次调用。"""

    def __init__(self, backup: BaseChatModel) -> None:
        super().__init__()
        self._backup = backup

    def _with_backup(self, request: ModelRequest) -> ModelRequest:
        return request.override(model=self._backup)

    def wrap_model_call(self, request, handler):
        METRICS.llm_call()
        try:
            return handler(request)
        except Exception as exc:  # noqa: BLE001 — 兜底：任何失败都值得换模型再试一次
            reason = classify_failure(exc)
            METRICS.llm_failure(reason)
            METRICS.model_fallback(reason)
            logger.warning("主模型调用失败(%s)，切换备用模型重试: %s", reason, exc)
            METRICS.llm_call()
            try:
                return handler(self._with_backup(request))
            except Exception as backup_exc:  # noqa: BLE001
                METRICS.llm_failure(classify_failure(backup_exc))
                raise

    async def awrap_model_call(self, request, handler):
        METRICS.llm_call()
        try:
            return await handler(request)
        except Exception as exc:  # noqa: BLE001
            reason = classify_failure(exc)
            METRICS.llm_failure(reason)
            METRICS.model_fallback(reason)
            logger.warning("主模型调用失败(%s)，切换备用模型重试: %s", reason, exc)
            METRICS.llm_call()
            try:
                return await handler(self._with_backup(request))
            except Exception as backup_exc:  # noqa: BLE001
                METRICS.llm_failure(classify_failure(backup_exc))
                raise


class ModelCallObserverMiddleware(AgentMiddleware):
    """只做可观测：统计模型调用次数与失败原因，不改变任何行为。"""

    def wrap_model_call(self, request, handler):
        METRICS.llm_call()
        try:
            return handler(request)
        except Exception as exc:  # noqa: BLE001
            METRICS.llm_failure(classify_failure(exc))
            raise

    async def awrap_model_call(self, request, handler):
        METRICS.llm_call()
        try:
            return await handler(request)
        except Exception as exc:  # noqa: BLE001
            METRICS.llm_failure(classify_failure(exc))
            raise


def build_model_middlewares(settings: Settings, backup: BaseChatModel | None) -> list[AgentMiddleware]:
    """按配置选择降级中间件（或退化成纯观测中间件）。"""
    if backup is None:
        logger.info("未配置备用模型：不启用降级，仅统计模型调用")
        return [ModelCallObserverMiddleware()]
    return [FallbackModelMiddleware(backup)]
