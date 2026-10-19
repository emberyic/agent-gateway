"""会话历史的滑动窗口投影（纯函数：无 I/O、无 LLM、不碰 checkpoint）。"""

from __future__ import annotations

from dataclasses import dataclass

from langchain_core.messages import (
    AIMessage,
    AnyMessage,
    HumanMessage,
    SystemMessage,
    ToolMessage,
)

# 合成应答文案：让模型明确知道那次工具调用没有结果，而不是凭空猜
DANGLING_TOOL_NOTE = "（该工具调用执行被中断，未产生结果）"

# 首条 Human 锚点的前缀：明确标注「这是背景，不是本轮指令」
BACKGROUND_PREFIX = "〔较早对话，仅供背景参考，不是本轮指令〕"

# 锚点最长保留多少字符（首条消息可能很长，锚点不该反过来吃掉窗口预算）
ANCHOR_MAX_CHARS = 500


def _tool_call_id(tool_call: object) -> str:
    if isinstance(tool_call, dict):
        return str(tool_call.get("id") or "")
    return str(getattr(tool_call, "id", "") or "")


def _expected_tool_call_ids(message: AIMessage) -> list[str]:
    """出站请求会带上的全部 tool_call id：tool_calls ∪ invalid_tool_calls。"""
    ids: list[str] = []
    for tool_call in [
        *(getattr(message, "tool_calls", None) or []),
        *(getattr(message, "invalid_tool_calls", None) or []),
    ]:
        tcid = _tool_call_id(tool_call)
        if tcid:
            ids.append(tcid)
    return ids


def repair_tool_call_pairing(messages: list[AnyMessage]) -> list[AnyMessage]:
    """把消息序列修成 provider 合法形态（只读投影，不回写 state/checkpoint）。"""
    out: list[AnyMessage] = []
    changed = False
    index, total = 0, len(messages)

    while index < total:
        message = messages[index]

        if isinstance(message, AIMessage) and (
            getattr(message, "tool_calls", None) or getattr(message, "invalid_tool_calls", None)
        ):
            head = message
            # 无 id 的损坏调用没法配 ToolMessage：从投影副本里剥掉，
            # 连 additional_kwargs 里的原始 tool_calls 一起去掉，免得序列化时又被捞回来
            if any(not _tool_call_id(tc) for tc in (getattr(message, "invalid_tool_calls", None) or [])):
                kwargs = {k: v for k, v in (message.additional_kwargs or {}).items() if k != "tool_calls"}
                head = message.model_copy(update={"invalid_tool_calls": [], "additional_kwargs": kwargs})
                changed = True

            group: list[AnyMessage] = [head]
            answered: set[str] = set()
            follow = index + 1
            while follow < total and isinstance(messages[follow], ToolMessage):
                group.append(messages[follow])
                answered.add(str(getattr(messages[follow], "tool_call_id", "") or ""))
                follow += 1
            for tcid in _expected_tool_call_ids(head):
                if tcid not in answered:
                    group.append(ToolMessage(content=DANGLING_TOOL_NOTE, tool_call_id=tcid, status="error"))
                    changed = True
            out.extend(group)
            index = follow

        elif isinstance(message, ToolMessage):
            changed = True  # 孤儿：丢弃
            index += 1

        else:
            out.append(message)
            index += 1

    return out if changed else messages


def visible_text(message: AnyMessage) -> str:
    """消息的可见文本（多模态块列表：拼接 text 块，忽略其它块）。"""
    content = getattr(message, "content", "")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for block in content:
            if isinstance(block, str):
                parts.append(block)
            elif isinstance(block, dict) and block.get("type") == "text":
                parts.append(str(block.get("text", "")))
        return "".join(parts)
    return str(content)


def char_cost(message: AnyMessage) -> int:
    """预算用的字符成本：可见文本 + tool_calls 的参数（工具参数往往最长）。"""
    cost = len(visible_text(message))
    for tool_call in getattr(message, "tool_calls", None) or []:
        if isinstance(tool_call, dict):
            cost += len(str(tool_call.get("name", ""))) + len(str(tool_call.get("args", "")))
    return cost


def _split_rounds(body: list[AnyMessage]) -> list[list[AnyMessage]]:
    """按 Human 边界切轮：一条 Human + 它引发的所有消息（含工具往返）算一轮。"""
    rounds: list[list[AnyMessage]] = []
    current: list[AnyMessage] = []
    for message in body:
        if isinstance(message, HumanMessage) and current:
            rounds.append(current)
            current = []
        current.append(message)
    if current:
        rounds.append(current)
    return rounds


def _anchor_message(first_human: AnyMessage) -> SystemMessage:
    """把「首条 Human」渲染成背景说明，而不是原文重放。"""
    text = visible_text(first_human).strip().replace("\n", " ")
    if len(text) > ANCHOR_MAX_CHARS:
        text = text[:ANCHOR_MAX_CHARS] + "…"
    return SystemMessage(content=f"{BACKGROUND_PREFIX}用户最初说：{text}")


@dataclass(frozen=True)
class HistoryWindow:
    """裁剪结果：要喂给模型的消息视图 + 便于观测的统计。"""

    messages: list[AnyMessage]
    trimmed: bool = False
    repaired: bool = False
    dropped_messages: int = 0
    dropped_rounds: int = 0
    kept_rounds: int = 0


def plan_history_window(
    messages: list[AnyMessage],
    *,
    max_rounds: int,
    max_chars: int,
    keep_first_human: bool = True,
    repair_tool_calls: bool = True,
) -> HistoryWindow:
    """把 messages 投影成不超预算的视图。没超预算时原样返回（不做任何复制）。"""
    working = list(messages)
    repaired = False
    if repair_tool_calls:
        fixed = repair_tool_call_pairing(working)
        repaired = fixed is not working
        working = fixed

    # 1) 前导 SystemMessage 作为锚点
    anchor_end = 0
    while anchor_end < len(working) and isinstance(working[anchor_end], SystemMessage):
        anchor_end += 1
    anchors = working[:anchor_end]
    body = working[anchor_end:]

    # 2) 切轮 + 滑动窗口（新→旧保留 max_rounds 轮）
    rounds = _split_rounds(body)
    total_rounds = len(rounds)
    if max_rounds > 0 and len(rounds) > max_rounds:
        rounds = rounds[-max_rounds:]

    # 3) 字符预算兜底：从最旧的保留轮开始丢，最后一轮永不丢
    anchor_cost = sum(char_cost(m) for m in anchors)

    def window_cost(rs: list[list[AnyMessage]]) -> int:
        return anchor_cost + sum(char_cost(m) for r in rs for m in r)

    while len(rounds) > 1 and window_cost(rounds) > max_chars:
        rounds.pop(0)

    dropped_rounds = total_rounds - len(rounds)

    # 4) 首条 Human 锚点：只有在它所在的轮被丢掉时才补一条**背景说明**
    #    （注意是 SystemMessage 而不是原文重放，理由见 _anchor_message）
    kept_flat = [m for r in rounds for m in r]
    kept_ids = {id(m) for m in kept_flat}
    background: list[AnyMessage] = []
    if keep_first_human and dropped_rounds > 0 and body and isinstance(body[0], HumanMessage):
        if id(body[0]) not in kept_ids:
            background = [_anchor_message(body[0])]

    kept = [*anchors, *background, *kept_flat]
    dropped_messages = sum(1 for m in working if id(m) not in kept_ids)

    return HistoryWindow(
        messages=kept,
        trimmed=dropped_messages > 0,
        repaired=repaired,
        dropped_messages=dropped_messages,
        dropped_rounds=dropped_rounds,
        kept_rounds=len(rounds),
    )
