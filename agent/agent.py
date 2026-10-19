"""Agent 装配：工具集 + 模型（含降级）+ 历史裁剪中间件 + SQLite checkpointer。"""

from __future__ import annotations

import logging

from langchain.agents import create_agent
from langchain.agents.middleware import AgentMiddleware, ModelRequest
from langchain_core.tools import BaseTool
from langgraph.graph.state import CompiledStateGraph

from history import plan_history_window
from knowledge import build_knowledge_tool
from metrics import METRICS
from models import build_fallback, build_model_middlewares, build_primary
from settings import Settings
from tools import build_time_tool, build_weather_tool

logger = logging.getLogger("agent.agent")

# 系统提示词：人设 + 三条工具路由规则
SYSTEM_PROMPT = """你是一个幽默且硬核的助手，说话要犀利。
            注意：知识库里记录的是【用户】的个人信息、项目经历和简历，不是你的。
            如果用户让你自我介绍，你要说你是“AI助手小鲸鱼”；只有用户问“我”的经历时，你才去检索并告诉用户关于他的信息。
            如果用户询问关于个人信息、项目经历、技能或简历细节，请调用 search_knowledge 工具。
            如果用户问天气，请调用 get_weather。
            如果用户问时间，请调用 get_current_time。"""


class HistoryTrimMiddleware(AgentMiddleware):
    """把喂给模型的历史裁成有界视图（滑动窗口），防止上下文 token 爆炸。"""

    def __init__(self, settings: Settings) -> None:
        super().__init__()
        self._settings = settings

    def _project(self, request: ModelRequest) -> ModelRequest:
        plan = plan_history_window(
            list(request.messages),
            max_rounds=self._settings.history_max_rounds,
            max_chars=self._settings.history_max_chars,
            keep_first_human=self._settings.history_keep_first_human,
            repair_tool_calls=self._settings.history_repair_tool_calls,
        )
        if not plan.trimmed and not plan.repaired:
            return request

        if plan.repaired:
            logger.warning("入模型前修复了工具调用配对（悬空/损坏/孤儿），本次不落库")
        if plan.trimmed:
            METRICS.history_trimmed(plan.dropped_messages)
            logger.info(
                "历史裁剪：保留最近 %d 轮，丢弃 %d 轮 / %d 条消息（checkpoint 不受影响）",
                plan.kept_rounds,
                plan.dropped_rounds,
                plan.dropped_messages,
            )
        return request.override(messages=plan.messages)

    def wrap_model_call(self, request, handler):
        return handler(self._project(request))

    async def awrap_model_call(self, request, handler):
        return await handler(self._project(request))


def build_tools(settings: Settings, retriever) -> list[BaseTool]:
    """装配工具集：本地工具 + 知识库检索工具。"""
    tools: list[BaseTool] = [
        build_time_tool(),
        build_weather_tool(settings.weather_api_key, settings.weather_host),
        build_knowledge_tool(retriever),
    ]
    logger.info("工具集: %s", [t.name for t in tools])
    return tools


def build_middlewares(settings: Settings, backup) -> list[AgentMiddleware]:
    """历史裁剪 + 模型降级/观测。"""
    return [
        HistoryTrimMiddleware(settings),
        *build_model_middlewares(settings, backup),
    ]


def build_agent(*, settings: Settings, checkpointer, retriever) -> CompiledStateGraph:
    """装配 agent（主模型 + 工具 + 两个中间件 + checkpointer）。"""
    tools = build_tools(settings, retriever)
    primary = build_primary(settings)
    backup = build_fallback(settings)

    agent = create_agent(
        model=primary,
        tools=tools,
        system_prompt=SYSTEM_PROMPT,
        middleware=build_middlewares(settings, backup),
        checkpointer=checkpointer,
    )
    logger.info(
        "agent 装配完成: model=%s 降级=%s 历史窗口=%d 轮/%d 字符",
        settings.model,
        settings.fallback_model if settings.fallback_enabled else "未启用",
        settings.history_max_rounds,
        settings.history_max_chars,
    )
    return agent
