#!/usr/bin/env python3
"""
claude_smoke.py — 用 Anthropic 官方 SDK 模拟 Claude Code 打真实 bridge。

覆盖 Claude Code 会用到的东西：
  非流式、流式、tool_use 往返、count_tokens、system 数组（cache_control）、
  anthropic-beta 头。

用法：
  export ANTHROPIC_BASE_URL=http://127.0.0.1:2810
  export ANTHROPIC_API_KEY=sk-bridge-dev
  BRIDGE_MODEL=opencode-go/deepseek-v4.1-flash:max python3 tests/claude_smoke.py
"""
import os
import sys
import uuid

from anthropic import Anthropic

BASE = os.environ.get("ANTHROPIC_BASE_URL", "http://127.0.0.1:2810")
KEY = os.environ.get("ANTHROPIC_API_KEY", "sk-bridge-dev")
MODEL = os.environ.get("BRIDGE_MODEL", "opencode-go/deepseek-v4.1-flash:max")

client = Anthropic(base_url=BASE, api_key=KEY, timeout=180.0, max_retries=0)

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(f"  [{'PASS' if ok else 'FAIL'}] {name}" + (f" — {detail}" if detail else ""))


def sid(tag):
    return f"claude-{tag}-{uuid.uuid4().hex[:8]}"


WEATHER = {"name": "get_weather", "description": "查询指定城市的当前天气",
           "input_schema": {"type": "object", "properties": {"city": {"type": "string"}},
                            "required": ["city"]}}


def t_nonstream():
    print("\n== messages.create（非流式）==")
    try:
        r = client.messages.create(
            model=MODEL, max_tokens=128, system="你是助手",
            messages=[{"role": "user", "content": "只回两个字：你好"}],
            extra_headers={"X-Session-ID": sid("basic")})
        text = "".join(b.text for b in r.content if b.type == "text")
        check("type=message & text", r.type == "message" and text != "", repr(text[:40]))
        check("stop_reason=end_turn", r.stop_reason == "end_turn", str(r.stop_reason))
        check("usage present", r.usage is not None and r.usage.output_tokens > 0, str(r.usage))
    except Exception as e:
        check("nonstream", False, repr(e))


def t_stream():
    print("\n== messages.stream（流式）==")
    try:
        got = ""
        with client.messages.stream(
                model=MODEL, max_tokens=128,
                messages=[{"role": "user", "content": "从1数到3，每行一个数字"}],
                extra_headers={"X-Session-ID": sid("stream")}) as s:
            for t in s.text_stream:
                got += t
        check("streamed text", len(got) > 0, repr(got[:60]))
    except Exception as e:
        check("stream", False, repr(e))


def t_system_blocks_and_beta():
    print("\n== system 数组(cache_control) + anthropic-beta ==")
    try:
        r = client.messages.create(
            model=MODEL, max_tokens=128,
            system=[{"type": "text", "text": "你是助手", "cache_control": {"type": "ephemeral"}}],
            messages=[{"role": "user", "content": "只回：ok"}],
            extra_headers={"X-Session-ID": sid("sys"), "anthropic-beta": "prompt-caching-2024-07-31"})
        text = "".join(b.text for b in r.content if b.type == "text")
        check("system blocks + beta accepted", text != "", repr(text[:30]))
    except Exception as e:
        check("system blocks", False, repr(e))


def t_tool_use():
    print("\n== tool_use 往返 ==")
    try:
        s = sid("tool")
        p = "必须调用 get_weather 查询北京天气，拿到结果后一句话回答。"
        r1 = client.messages.create(model=MODEL, max_tokens=256, tools=[WEATHER],
                                    messages=[{"role": "user", "content": p}],
                                    extra_headers={"X-Session-ID": s})
        tu = next((b for b in r1.content if b.type == "tool_use"), None)
        check("stop_reason=tool_use", r1.stop_reason == "tool_use" and tu is not None,
              f"stop={r1.stop_reason}")
        if tu is None:
            return
        check("tool_use has id/name/input", bool(tu.id and tu.name and tu.input is not None),
              f"{tu.name} {tu.input}")
        r2 = client.messages.create(
            model=MODEL, max_tokens=256, tools=[WEATHER],
            messages=[{"role": "user", "content": p},
                      {"role": "assistant", "content": [tu]},
                      {"role": "user", "content": [{"type": "tool_result",
                                                    "tool_use_id": tu.id, "content": "北京 晴 26℃"}]}],
            extra_headers={"X-Session-ID": s})
        text = "".join(b.text for b in r2.content if b.type == "text")
        has_tool = any(b.type == "tool_use" for b in r2.content)
        # 回填后模型可能给最终答案(end_turn)，也可能继续调工具(tool_use)——都算往返成功
        check("round-trip after tool_result",
              r2.stop_reason in ("end_turn", "tool_use"), f"stop={r2.stop_reason}")
    except Exception as e:
        check("tool_use", False, repr(e))


def t_count_tokens():
    print("\n== messages.count_tokens ==")
    try:
        r = client.messages.count_tokens(
            model=MODEL, messages=[{"role": "user", "content": "hello world, 你好"}])
        check("count_tokens returns input_tokens", r.input_tokens > 0, str(r.input_tokens))
    except Exception as e:
        check("count_tokens", False, repr(e))


TESTS = {
    "nonstream": t_nonstream,
    "stream": t_stream,
    "system_blocks": t_system_blocks_and_beta,
    "tool_use": t_tool_use,
    "count_tokens": t_count_tokens,
}

if __name__ == "__main__":
    names = sys.argv[1:] or list(TESTS)
    print(f"base={BASE}  model={MODEL}")
    for n in names:
        if n not in TESTS:
            print(f"unknown test {n!r}; choose from {list(TESTS)}")
            sys.exit(2)
        TESTS[n]()
    print(f"\n===== {len(PASS)} passed, {len(FAIL)} failed =====")
    if FAIL:
        print("failed:", ", ".join(FAIL))
    sys.exit(1 if FAIL else 0)
