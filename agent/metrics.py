"""极简指标收集 + Prometheus 文本输出（零依赖）。"""

from __future__ import annotations

import threading
import time

# 运行时长直方图的分桶（秒）。覆盖「一次对话」的典型区间
RUN_DURATION_BUCKETS = (1.0, 3.0, 10.0, 30.0, 60.0, 120.0)


class Metrics:
    """线程安全的计数器/仪表盘集合。"""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._started_at = time.time()

        self.requests_total = 0
        self.streams_total = 0
        self.streams_in_flight = 0
        self.sse_frames_total = 0
        self.heartbeats_total = 0

        self.llm_calls_total = 0
        self.llm_failures_total = 0
        self.model_fallbacks_total = 0

        self.history_trims_total = 0
        self.history_messages_dropped_total = 0

        self.errors_total = 0
        self.runs_total = 0
        self.run_duration_sum = 0.0
        self.run_duration_count = 0
        self._run_duration_buckets = dict.fromkeys(RUN_DURATION_BUCKETS, 0)

        self._tool_results: dict[str, int] = {}
        self._error_codes: dict[str, int] = {}
        self._llm_failure_reasons: dict[str, int] = {}

    # —— 埋点 ——

    def inc_request(self) -> None:
        with self._lock:
            self.requests_total += 1

    def stream_started(self) -> None:
        with self._lock:
            self.streams_total += 1
            self.streams_in_flight += 1

    def stream_finished(self) -> None:
        with self._lock:
            self.streams_in_flight = max(0, self.streams_in_flight - 1)

    def inc_frame(self) -> None:
        with self._lock:
            self.sse_frames_total += 1

    def inc_heartbeat(self) -> None:
        with self._lock:
            self.heartbeats_total += 1

    def llm_call(self) -> None:
        with self._lock:
            self.llm_calls_total += 1

    def llm_failure(self, reason: str) -> None:
        with self._lock:
            self.llm_failures_total += 1
            self._llm_failure_reasons[reason] = self._llm_failure_reasons.get(reason, 0) + 1

    def model_fallback(self, reason: str) -> None:
        """主模型失败、切换到备用模型重试。"""
        with self._lock:
            self.model_fallbacks_total += 1

    def history_trimmed(self, dropped_messages: int) -> None:
        with self._lock:
            self.history_trims_total += 1
            self.history_messages_dropped_total += dropped_messages

    def tool_result(self, tool_name: str) -> None:
        with self._lock:
            name = tool_name or "unknown"
            self._tool_results[name] = self._tool_results.get(name, 0) + 1

    def error(self, code: str) -> None:
        with self._lock:
            self.errors_total += 1
            self._error_codes[code] = self._error_codes.get(code, 0) + 1

    def observe_run(self, seconds: float) -> None:
        with self._lock:
            self.runs_total += 1
            self.run_duration_sum += seconds
            self.run_duration_count += 1
            for bound in RUN_DURATION_BUCKETS:
                if seconds <= bound:
                    self._run_duration_buckets[bound] += 1

    # —— 输出 ——

    def render(self) -> str:
        """Prometheus 文本格式（text/plain; version=0.0.4）。"""
        with self._lock:
            snap = {
                "requests_total": self.requests_total,
                "streams_total": self.streams_total,
                "streams_in_flight": self.streams_in_flight,
                "sse_frames_total": self.sse_frames_total,
                "heartbeats_total": self.heartbeats_total,
                "llm_calls_total": self.llm_calls_total,
                "llm_failures_total": self.llm_failures_total,
                "model_fallbacks_total": self.model_fallbacks_total,
                "history_trims_total": self.history_trims_total,
                "history_messages_dropped_total": self.history_messages_dropped_total,
                "errors_total": self.errors_total,
                "runs_total": self.runs_total,
                "run_duration_sum": self.run_duration_sum,
                "run_duration_count": self.run_duration_count,
                "uptime_seconds": time.time() - self._started_at,
            }
            buckets = dict(self._run_duration_buckets)
            tools = dict(self._tool_results)
            codes = dict(self._error_codes)
            reasons = dict(self._llm_failure_reasons)

        out: list[str] = []

        def counter(name: str, help_text: str, value: float) -> None:
            out.append(f"# HELP {name} {help_text}")
            out.append(f"# TYPE {name} counter")
            out.append(f"{name} {_num(value)}")

        def gauge(name: str, help_text: str, value: float) -> None:
            out.append(f"# HELP {name} {help_text}")
            out.append(f"# TYPE {name} gauge")
            out.append(f"{name} {_num(value)}")

        counter("agent_requests_total", "HTTP 请求总数（含健康检查与指标抓取）", snap["requests_total"])
        counter("agent_streams_total", "累计开始的 SSE 对话流数", snap["streams_total"])
        gauge("agent_streams_in_flight", "当前正在进行中的 SSE 对话流数", snap["streams_in_flight"])
        counter("agent_sse_frames_total", "写给客户端的 SSE 内容帧数", snap["sse_frames_total"])
        counter("agent_heartbeats_total", "写给客户端的 SSE 心跳数（空闲保活帧）", snap["heartbeats_total"])
        counter("agent_llm_calls_total", "模型调用次数（含失败与降级重试）", snap["llm_calls_total"])
        counter("agent_llm_failures_total", "模型调用失败次数", snap["llm_failures_total"])
        counter("agent_model_fallbacks_total", "切换到备用模型的次数（降级次数）", snap["model_fallbacks_total"])
        counter("agent_history_trims_total", "触发历史裁剪的次数", snap["history_trims_total"])
        counter(
            "agent_history_messages_dropped_total",
            "被滑动窗口裁掉的消息总数（只影响喂给模型的视图，checkpoint 不删）",
            snap["history_messages_dropped_total"],
        )
        counter("agent_errors_total", "按错误码统计的失败次数", snap["errors_total"])
        counter("agent_runs_total", "完成的对话轮数", snap["runs_total"])
        gauge("agent_uptime_seconds", "进程运行时长（秒）", snap["uptime_seconds"])
        gauge("agent_up", "进程存活（恒为 1）", 1)

        out.append("# HELP agent_run_duration_seconds 单轮对话耗时分布")
        out.append("# TYPE agent_run_duration_seconds histogram")
        for bound in RUN_DURATION_BUCKETS:
            out.append(f'agent_run_duration_seconds_bucket{{le="{_num(bound)}"}} {buckets.get(bound, 0)}')
        out.append(f'agent_run_duration_seconds_bucket{{le="+Inf"}} {snap["run_duration_count"]}')
        out.append(f"agent_run_duration_seconds_sum {_num(snap['run_duration_sum'])}")
        out.append(f"agent_run_duration_seconds_count {snap['run_duration_count']}")

        if tools:
            out.append("# HELP agent_tool_results_total 按工具名统计的工具返回次数")
            out.append("# TYPE agent_tool_results_total counter")
            for name in sorted(tools):
                out.append(f'agent_tool_results_total{{tool="{_escape(name)}"}} {tools[name]}')

        if reasons:
            out.append("# HELP agent_llm_failure_reasons_total 按原因统计的模型失败次数")
            out.append("# TYPE agent_llm_failure_reasons_total counter")
            for reason in sorted(reasons):
                out.append(f'agent_llm_failure_reasons_total{{reason="{_escape(reason)}"}} {reasons[reason]}')

        if codes:
            out.append("# HELP agent_error_codes_total 按错误码统计的失败次数")
            out.append("# TYPE agent_error_codes_total counter")
            for code in sorted(codes):
                out.append(f'agent_error_codes_total{{code="{_escape(code)}"}} {codes[code]}')

        return "\n".join(out) + "\n"


def _num(value: float) -> str:
    """整数不带小数点，浮点保留必要精度。"""
    if isinstance(value, int) or float(value).is_integer():
        return str(int(value))
    return f"{value:.3f}"


def _escape(value: str) -> str:
    """转义 Prometheus 标签值里的反斜杠、引号和换行。"""
    return value.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


# 进程级单例：埋点方直接 METRICS.inc_frame() 式调用
METRICS = Metrics()
