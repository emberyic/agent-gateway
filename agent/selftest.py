"""离线自检：不需要 API Key、不联网、不起服务。"""

from __future__ import annotations

import asyncio
import logging
import sqlite3
import time

from langchain_core.messages import AIMessage, HumanMessage, SystemMessage, ToolMessage

import health
import metrics
from history import plan_history_window, repair_tool_call_pairing
from settings import load_settings


def _tool_call(call_id: str, name: str = "get_weather") -> dict:
    return {"name": name, "args": {"city": "上海"}, "id": call_id, "type": "tool_call"}


def _two_round_history() -> list:
    """两轮对话，第一轮里带一次完整的工具往返。"""
    return [
        HumanMessage(content="上海天气怎么样"),
        AIMessage(content="", tool_calls=[_tool_call("call_1")]),
        ToolMessage(content="上海 晴 25℃", tool_call_id="call_1", name="get_weather"),
        AIMessage(content="上海今天晴，25℃。"),
        HumanMessage(content="那明天呢"),
        AIMessage(content="明天也差不多。"),
    ]


# history：滑动窗口


def test_window_keeps_recent_rounds_and_first_human() -> None:
    messages = _two_round_history()
    plan = plan_history_window(messages, max_rounds=1, max_chars=100000)

    assert plan.trimmed is True
    assert plan.dropped_rounds == 1
    # 背景说明（SystemMessage 锚点）+ 最近一轮
    assert [type(m).__name__ for m in plan.messages] == ["SystemMessage", "HumanMessage", "AIMessage"]
    assert "上海天气怎么样" in plan.messages[0].content
    assert plan.messages[-1].content == "明天也差不多。"


def test_first_human_anchor_is_background_not_a_new_instruction() -> None:
    """回归测试（实测踩过）：被裁掉的首条 Human 不能以原始消息形式重放。"""
    messages = [
        HumanMessage(content='第一轮：记住 A=1，只回复"好"'),
        AIMessage(content="好"),
        HumanMessage(content="第二轮：记住 B=2，只回复好"),
        AIMessage(content="好"),
        HumanMessage(content="用三点列出 Go 语言的优点"),
    ]
    plan = plan_history_window(messages, max_rounds=1, max_chars=100000)

    human_messages = [m for m in plan.messages if isinstance(m, HumanMessage)]
    assert [m.content for m in human_messages] == ["用三点列出 Go 语言的优点"], "窗口里只能有本轮这一条 Human"

    background = [m for m in plan.messages if isinstance(m, SystemMessage)]
    assert background, "应保留一条背景说明"
    assert "不是本轮指令" in background[0].content
    assert '只回复"好"' in background[0].content


def test_window_can_drop_the_first_human_anchor() -> None:
    messages = _two_round_history()
    plan = plan_history_window(messages, max_rounds=1, max_chars=100000, keep_first_human=False)
    assert [m.content for m in plan.messages if isinstance(m, HumanMessage)] == ["那明天呢"]


def test_window_never_splits_tool_calls_from_their_results() -> None:
    """最重要的一条不变量：配对要么整组留下、要么整组丢掉。"""
    messages = _two_round_history()

    # 保留第一轮（工具往返在里面）→ 必须成对出现
    keep_first = plan_history_window(messages, max_rounds=1, max_chars=100000, keep_first_human=True)
    kept_calls = [m for m in keep_first.messages if isinstance(m, AIMessage) and m.tool_calls]
    kept_results = [m for m in keep_first.messages if isinstance(m, ToolMessage)]
    assert len(kept_calls) == len(kept_results), "工具调用与结果必须成对"

    # 丢掉第一轮 → 两者都不该出现（不能只剩悬空调用）
    drop_first = plan_history_window(messages, max_rounds=1, max_chars=100000, keep_first_human=False)
    assert not [m for m in drop_first.messages if isinstance(m, AIMessage) and m.tool_calls]
    assert not [m for m in drop_first.messages if isinstance(m, ToolMessage)]


def test_window_applies_char_budget() -> None:
    messages = [
        HumanMessage(content="第一轮"),
        AIMessage(content="x" * 200),
        HumanMessage(content="第二轮"),
        AIMessage(content="y" * 200),
    ]
    plan = plan_history_window(messages, max_rounds=2, max_chars=120, keep_first_human=False)
    # 预算只够最后一轮：更旧的那轮被丢掉，但最后一轮永远保留
    assert [m.content for m in plan.messages] == ["第二轮", "y" * 200]
    assert plan.dropped_rounds == 1


def test_window_leaves_short_history_untouched() -> None:
    messages = _two_round_history()
    plan = plan_history_window(messages, max_rounds=8, max_chars=100000)
    assert plan.trimmed is False
    assert plan.messages == messages
    assert plan.messages[0] is messages[0], "没超预算时不应复制/改写消息对象"


def test_window_keeps_system_anchor() -> None:
    messages = [SystemMessage(content="你是助手"), *_two_round_history()]
    plan = plan_history_window(messages, max_rounds=1, max_chars=100000)
    assert isinstance(plan.messages[0], SystemMessage)
    assert plan.messages[0].content == "你是助手"
    # 背景说明排在系统提示词之后
    assert isinstance(plan.messages[1], SystemMessage)
    assert "较早对话" in plan.messages[1].content


# history：工具调用配对修复


def test_repair_adds_missing_tool_result() -> None:
    messages = [
        HumanMessage(content="查天气"),
        AIMessage(content="", tool_calls=[_tool_call("call_x")]),
    ]
    fixed = repair_tool_call_pairing(messages)
    assert len(fixed) == 3
    assert isinstance(fixed[-1], ToolMessage)
    assert fixed[-1].tool_call_id == "call_x"
    assert fixed[-1].status == "error"


def test_repair_drops_orphan_tool_message() -> None:
    messages = [
        HumanMessage(content="你好"),
        ToolMessage(content="没有对应的调用", tool_call_id="call_ghost"),
    ]
    fixed = repair_tool_call_pairing(messages)
    assert [type(m).__name__ for m in fixed] == ["HumanMessage"]


def test_repair_strips_invalid_tool_calls_without_id() -> None:
    messages = [
        HumanMessage(content="查天气"),
        AIMessage(
            content="",
            tool_calls=[],
            invalid_tool_calls=[{"name": "get_weather", "args": "{坏 JSON", "id": None, "error": "bad json"}],
        ),
    ]
    fixed = repair_tool_call_pairing(messages)
    ai = [m for m in fixed if isinstance(m, AIMessage)][0]
    assert ai.invalid_tool_calls == []
    assert not [m for m in fixed if isinstance(m, ToolMessage)]


def test_repair_leaves_healthy_history_alone() -> None:
    messages = [
        HumanMessage(content="上海天气怎么样"),
        AIMessage(content="", tool_calls=[_tool_call("call_1")]),
        ToolMessage(content="晴 25℃", tool_call_id="call_1", name="get_weather"),
        AIMessage(content="上海今天晴。"),
    ]
    assert repair_tool_call_pairing(messages) is messages


# metrics


def test_metrics_render_contains_expected_series() -> None:
    m = metrics.Metrics()
    m.inc_request()
    m.stream_started()
    m.inc_frame()
    m.llm_call()
    m.llm_failure("timeout")
    m.model_fallback("timeout")
    m.history_trimmed(dropped_messages=4)
    m.tool_result("get_weather")
    m.observe_run(2.5)
    m.stream_finished()

    text = m.render()
    for expected in (
        "# TYPE agent_requests_total counter",
        "agent_requests_total 1",
        'agent_tool_results_total{tool="get_weather"} 1',
        'agent_llm_failure_reasons_total{reason="timeout"} 1',
        "agent_model_fallbacks_total 1",
        "agent_history_messages_dropped_total 4",
        "agent_run_duration_seconds_count 1",
        "agent_streams_in_flight 0",
    ):
        assert expected in text, f"指标输出缺少: {expected}"
    # 直方图分桶必须完整（含 +Inf）
    assert 'agent_run_duration_seconds_bucket{le="+Inf"} 1' in text


# health


def test_run_checks_aggregates_results() -> None:
    async def ok() -> None:
        return None

    async def bad() -> None:
        raise RuntimeError("依赖挂了")

    healthy, body = asyncio.run(health.run_checks({"a": ok, "b": ok}))
    assert healthy is True
    assert body == {"a": "ok", "b": "ok"}

    healthy, body = asyncio.run(health.run_checks({"a": ok, "b": bad}))
    assert healthy is False
    assert body["a"] == "ok"
    assert "依赖挂了" in body["b"]


def test_run_checks_times_out_slow_check() -> None:
    async def slow() -> None:
        await asyncio.sleep(1)

    healthy, body = asyncio.run(health.run_checks({"slow": slow}, timeout=0.05))
    assert healthy is False
    assert "超时" in body["slow"]


def test_check_sqlite_is_readonly_and_does_not_create_file() -> None:
    """探活只读：文件不存在时报错且不创建，存在的库能读"""
    settings = load_settings()
    data_dir = settings.checkpoint_path.parent
    data_dir.mkdir(parents=True, exist_ok=True)

    missing = data_dir / "selftest_missing.db"
    if missing.exists():
        missing.unlink()
    try:
        health.check_sqlite(missing)
        raise AssertionError("文件不存在时应当报错")
    except sqlite3.OperationalError:
        pass
    assert not missing.exists(), "探活不应该创建文件"

    probe = data_dir / "selftest_probe.db"
    if probe.exists():
        probe.unlink()
    conn = sqlite3.connect(str(probe))
    conn.execute("CREATE TABLE IF NOT EXISTS t (x INTEGER)")
    conn.commit()
    conn.close()
    health.check_sqlite(probe)
    probe.unlink()


# SSE 帧与攒批策略


def test_sse_data_prefixes_every_line() -> None:
    from main import _sse_data

    frame = _sse_data("第一行\n第二行")
    assert frame == "data: 第一行\ndata: 第二行\n\n"
    # 单行输出与原来的格式完全一致（前端无需改动）
    assert _sse_data("你好") == "data: 你好\n\n"


def test_should_flush_on_punctuation_length_or_time() -> None:
    from main import _should_flush

    settings = load_settings()

    # 断句符（注意默认不含 "\n"：markdown 换行太频繁，带上它缓冲就废了）
    assert _should_flush("你好", "。", time.monotonic(), settings) is True
    assert _should_flush("你好", "\n", time.monotonic(), settings) is False
    # 长度
    long_buffer = "字" * settings.stream_flush_chars
    assert _should_flush(long_buffer, "字", time.monotonic(), settings) is True
    # 时间：即使没有标点、长度也不够，攒久了也要吐出去
    stale = time.monotonic() - settings.stream_flush_pause - 1
    assert _should_flush("一点点", "字", stale, settings) is True


def _main() -> int:
    # 有几条测试是**故意**制造失败的（坏依赖、慢依赖），health.py 会按设计记 warning
    # 自检时把 WARNING 以下静音，免得这类日志被当成测试失败（曾被误读，故注明）
    logging.disable(logging.WARNING)

    tests = [(name, fn) for name, fn in sorted(globals().items()) if name.startswith("test_") and callable(fn)]
    failed = 0
    for name, fn in tests:
        try:
            fn()
            print(f"  [OK]   {name}")
        except Exception as exc:  # noqa: BLE001 — 自检报告用
            failed += 1
            print(f"  [FAIL] {name}: {type(exc).__name__}: {exc}")
    print(f"\n{len(tests) - failed}/{len(tests)} 通过")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(_main())
