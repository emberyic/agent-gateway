"""运行配置：环境变量 / .env → 一个只读的 Settings 对象。"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

from dotenv import load_dotenv

# 本文件所在目录（即 agent/）。所有默认路径都基于它，
# 于是从任何工作目录启动都不会找不到文件
BASE_DIR = Path(__file__).resolve().parent

# 国内下载 embedding 模型走镜像。必须在 huggingface_hub 真正发请求之前生效，
# 所以放在模块顶层（本模块是应用里第一个被导入的）
os.environ.setdefault("HF_ENDPOINT", "https://hf-mirror.com")

load_dotenv(BASE_DIR / ".env")


def _str(name: str, default: str = "") -> str:
    value = os.getenv(name)
    return default if value is None or value.strip() == "" else value.strip()


def _int(name: str, default: int) -> int:
    raw = _str(name)
    if raw == "":
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise ValueError(f"环境变量 {name}={raw!r} 不是整数") from exc


def _float(name: str, default: float) -> float:
    raw = _str(name)
    if raw == "":
        return default
    try:
        return float(raw)
    except ValueError as exc:
        raise ValueError(f"环境变量 {name}={raw!r} 不是数字") from exc


def _bool(name: str, default: bool) -> bool:
    raw = _str(name).lower()
    if raw == "":
        return default
    return raw in ("1", "true", "yes", "on")


def _path(name: str, default: Path) -> Path:
    raw = _str(name)
    if raw == "":
        return default
    p = Path(raw)
    return p if p.is_absolute() else (BASE_DIR / p)


@dataclass(frozen=True)
class Settings:
    """全部运行参数。字段顺序即「配置分组」顺序。"""

    # —— 服务 ——
    host: str
    port: int

    # —— 主模型 ——
    model: str
    base_url: str
    api_key: str
    # 单次 HTTP 请求（含一次流式读）的超时；不是整轮对话的总时限
    llm_timeout: float
    # SDK 层重试次数（针对 429/5xx 这类瞬时错误）
    llm_max_retries: int

    # —— 备用模型（降级）——
    # 三个都配齐才启用；主模型失败（超时/网络/认证/5xx）时改用它重试同一次模型调用
    fallback_model: str
    fallback_base_url: str
    fallback_api_key: str
    fallback_timeout: float

    # —— 会话历史裁剪（滑动窗口）——
    history_max_rounds: int
    history_max_chars: int
    # 首条 Human 被裁掉时，是否补一条标注「仅供背景参考」的 SystemMessage
    # 注意补的是说明而不是原文 —— 原文重放会把用户最早的指令变成新指令（实测踩过）
    history_keep_first_human: bool
    # 入模型前修复悬空/损坏的 tool_calls（不修复会被 provider 400，且污染整个会话线程）
    history_repair_tool_calls: bool

    # —— 流式输出 ——
    stream_flush_chars: int
    # 攒批上限（秒）：即使没遇到断句符，超过这个时间也要把缓冲区吐出去
    stream_flush_pause: float
    # 视为「一句说完」的断句符。故意不含 "\n" —— markdown 列表里换行太多，
    # 带上它就等于每个 chunk 都 flush，缓冲形同虚设
    stream_flush_punct: str
    heartbeat_seconds: float

    # —— 工具 ——
    weather_api_key: str
    weather_host: str

    # —— 知识库 ——
    knowledge_file: Path
    embedding_model: str
    rag_top_k: int

    # —— 记忆（SQLite checkpoint，结构照搬原项目）——
    checkpoint_path: Path

    # —— 静态资源 ——
    assets_dir: Path

    @property
    def fallback_enabled(self) -> bool:
        """备用模型是否配置齐全。"""
        return bool(self.fallback_model and self.fallback_api_key)

    def describe(self) -> dict[str, object]:
        """给 /healthz 与启动日志用的可公开描述（不含任何密钥）。"""
        return {
            "model": self.model,
            "base_url": self.base_url,
            "llm_timeout_s": self.llm_timeout,
            "fallback_model": self.fallback_model if self.fallback_enabled else None,
            "history_max_rounds": self.history_max_rounds,
            "history_max_chars": self.history_max_chars,
            "heartbeat_s": self.heartbeat_seconds,
            "rag_top_k": self.rag_top_k,
            "checkpoint": str(self.checkpoint_path),
        }


def load_settings() -> Settings:
    """从环境变量 / .env 读出配置。启动时调用一次。"""
    return Settings(
        host=_str("AGENT_HOST", "127.0.0.1"),
        port=_int("AGENT_PORT", 8000),
        model=_str("AGENT_MODEL", "deepseek-chat"),
        base_url=_str("AGENT_BASE_URL", "https://api.deepseek.com"),
        api_key=_str("DEEPSEEK_API_KEY"),
        llm_timeout=_float("AGENT_LLM_TIMEOUT_SECONDS", 60.0),
        llm_max_retries=_int("AGENT_LLM_MAX_RETRIES", 1),
        fallback_model=_str("AGENT_FALLBACK_MODEL"),
        fallback_base_url=_str("AGENT_FALLBACK_BASE_URL", "https://api.deepseek.com"),
        fallback_api_key=_str("AGENT_FALLBACK_API_KEY"),
        fallback_timeout=_float("AGENT_FALLBACK_TIMEOUT_SECONDS", 60.0),
        history_max_rounds=_int("AGENT_HISTORY_MAX_ROUNDS", 8),
        history_max_chars=_int("AGENT_HISTORY_MAX_CHARS", 24000),
        history_keep_first_human=_bool("AGENT_HISTORY_KEEP_FIRST_HUMAN", True),
        history_repair_tool_calls=_bool("AGENT_HISTORY_REPAIR_TOOL_CALLS", True),
        stream_flush_chars=_int("AGENT_STREAM_FLUSH_CHARS", 200),
        stream_flush_pause=_float("AGENT_STREAM_FLUSH_PAUSE_SECONDS", 0.15),
        stream_flush_punct=_str("AGENT_STREAM_FLUSH_PUNCT", "。！？!?；;"),
        heartbeat_seconds=_float("AGENT_HEARTBEAT_SECONDS", 10.0),
        weather_api_key=_str("QWEATHER_API_KEY"),
        weather_host=_str("QWEATHER_HOST", "devapi.qweather.com"),
        knowledge_file=_path("AGENT_KNOWLEDGE_FILE", BASE_DIR / "knowledge" / "my_knowledge.txt"),
        embedding_model=_str("AGENT_EMBEDDING_MODEL", "BAAI/bge-small-zh-v1.5"),
        rag_top_k=_int("AGENT_RAG_TOP_K", 3),
        checkpoint_path=_path("AGENT_CHECKPOINT_PATH", BASE_DIR / "data" / "checkpoints.db"),
        assets_dir=_path("AGENT_ASSETS_DIR", BASE_DIR / "assets"),
    )
