#!/usr/bin/env python3
"""
compat_matrix.py — Janus 兼容性矩阵（重点：Tool Calling + Responses 事件）

用 OpenAI 官方 SDK + 原始 SSE 打真实 bridge，覆盖：
  工具调用：单个 / 多个并行 / 工具报错 / 超大结果 / 流式 / 带 reasoning
  Responses：SSE 事件序列 / function_call 事件 + 续链
  并发：多客户端不串会话

注意：每个 Chat 用例都带唯一 `user`，避免落到同一个会话 key 被缓存/上下文串扰
（这本身也是"会话隔离"的验证）。

用法：
  export BRIDGE_MODEL=opencode-go/deepseek-v4.1-flash:max
  python3 tests/compat_matrix.py            # 全部
  python3 tests/compat_matrix.py tools responses
"""
import json
import os
import sys
import time
import urllib.request
import uuid

from openai import OpenAI

BASE = os.environ.get("BRIDGE_URL", "http://127.0.0.1:2810/v1")
ROOT = BASE[:-3] if BASE.endswith("/v1") else BASE
KEY = os.environ.get("BRIDGE_API_KEY", "sk-bridge-dev")
MODEL = os.environ.get("BRIDGE_MODEL", "opencode-go/deepseek-v4.1-flash:max")

client = OpenAI(base_url=BASE, api_key=KEY, timeout=180.0, max_retries=0)

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(f"  [{'PASS' if ok else 'FAIL'}] {name}" + (f" — {detail}" if detail else ""))


def u(tag):
    """每个用例唯一 user，保证独立会话（也顺带验证隔离）。"""
    return f"{tag}-{uuid.uuid4().hex[:8]}"


WEATHER = {"type": "function", "function": {
    "name": "get_weather", "description": "查询指定城市的当前天气",
    "parameters": {"type": "object", "properties": {"city": {"type": "string"}},
                   "required": ["city"]}}}
CLOCK = {"type": "function", "function": {
    "name": "get_time", "description": "查询指定时区的当前时间",
    "parameters": {"type": "object", "properties": {"tz": {"type": "string"}},
                   "required": ["tz"]}}}
TOOLS = [WEATHER, CLOCK]


def asst_msg(resp):
    m = resp.choices[0].message
    out = {"role": "assistant", "content": m.content}
    if m.tool_calls:
        out["tool_calls"] = [
            {"id": tc.id, "type": "function",
             "function": {"name": tc.function.name, "arguments": tc.function.arguments}}
            for tc in m.tool_calls]
    return out


def tool_result(call_id, content):
    return {"role": "tool", "tool_call_id": call_id, "content": content}


def validate_tool_calls(tcs):
    problems, ids = [], []
    if not tcs:
        problems.append("no tool_calls")
    for tc in tcs:
        if not tc.id:
            problems.append("missing id")
        ids.append(tc.id)
        if tc.type != "function":
            problems.append(f"type={tc.type}")
        if not tc.function or not tc.function.name:
            problems.append("missing name")
        else:
            try:
                json.loads(tc.function.arguments or "{}")
            except Exception:
                problems.append("arguments not JSON")
    if len(set(ids)) != len(ids):
        problems.append("duplicate ids")
    return problems


# ---------------- 工具调用矩阵 ----------------

def t_tool_single():
    print("\n== tool: 单个 ==")
    try:
        mk = u("tool-single")
        p = "必须调用 get_weather 查询北京天气，拿到工具结果后用一句话回答。"
        msgs = [{"role": "user", "content": p}]
        r1 = client.chat.completions.create(model=MODEL, user=mk, tools=[WEATHER], messages=msgs)
        tcs = r1.choices[0].message.tool_calls or []
        if not tcs:
            check("single tool_call returned", False, f"finish={r1.choices[0].finish_reason}")
            return
        check("tool_call protocol valid", not validate_tool_calls(tcs))
        msgs.append(asst_msg(r1))
        msgs.append(tool_result(tcs[0].id, "北京 晴 26℃ 东南风3级"))
        r2 = client.chat.completions.create(model=MODEL, user=mk, tools=[WEATHER], messages=msgs)
        txt = r2.choices[0].message.content or ""
        check("final answer after result", txt != "" or r2.choices[0].finish_reason == "tool_calls",
              f"finish={r2.choices[0].finish_reason} content={repr(txt[:40])}")
    except Exception as e:
        check("tool single", False, repr(e))


def t_tool_multi():
    print("\n== tool: 多个（并行）==")
    try:
        mk = u("tool-multi")
        p = ("必须在一轮里同时调用 get_weather(北京) 和 get_time(CST) 两个工具，"
             "拿到全部结果后一句话总结。不要只调用一个。")
        r1 = client.chat.completions.create(model=MODEL, user=mk, tools=TOOLS,
                                            messages=[{"role": "user", "content": p}])
        tcs = r1.choices[0].message.tool_calls or []
        check(">=1 tool_call", len(tcs) >= 1, f"{len(tcs)} calls finish={r1.choices[0].finish_reason}")
        if not tcs:
            return
        if len(tcs) >= 2:
            check("parallel protocol valid", not validate_tool_calls(tcs), f"{len(tcs)} calls")
            check("distinct tool names", len({t.function.name for t in tcs}) == len(tcs),
                  str([t.function.name for t in tcs]))
        msgs = [{"role": "user", "content": p}, asst_msg(r1)]
        for tc in tcs:
            msgs.append(tool_result(tc.id, f"ok:{tc.function.name}"))
        r2 = client.chat.completions.create(model=MODEL, user=mk, tools=TOOLS, messages=msgs)
        fin = r2.choices[0].finish_reason
        txt = r2.choices[0].message.content or ""
        check("round-trip after all results", fin in ("stop", "tool_calls"),
              f"finish={fin} content={repr(txt[:40])}")
    except Exception as e:
        check("tool multi", False, repr(e))


def t_tool_error():
    print("\n== tool: 工具报错 ==")
    try:
        mk = u("tool-error")
        p = "必须调用 get_weather 查北京天气。"
        r1 = client.chat.completions.create(model=MODEL, user=mk, tools=[WEATHER],
                                            messages=[{"role": "user", "content": p}])
        tcs = r1.choices[0].message.tool_calls or []
        check("tool_error: got tool_call", len(tcs) >= 1, f"{len(tcs)}")
        if not tcs:
            return
        msgs = [{"role": "user", "content": p}, asst_msg(r1),
                tool_result(tcs[0].id, "[error] upstream timeout, tool unavailable")]
        r2 = client.chat.completions.create(model=MODEL, user=mk, tools=[WEATHER], messages=msgs)
        check("bridge survives tool error", r2.choices[0].finish_reason in ("stop", "tool_calls"),
              f"finish={r2.choices[0].finish_reason}")
    except Exception as e:
        check("tool error", False, repr(e))


def t_tool_large():
    print("\n== tool: 超大结果 ==")
    try:
        mk = u("tool-large")
        p = "必须调用 get_weather 查北京天气。"
        r1 = client.chat.completions.create(model=MODEL, user=mk, tools=[WEATHER],
                                            messages=[{"role": "user", "content": p}])
        tcs = r1.choices[0].message.tool_calls or []
        check("tool_large: got tool_call", len(tcs) >= 1, f"{len(tcs)}")
        if not tcs:
            return
        big = "行" + ("数据" * 20000)  # ~80KB
        msgs = [{"role": "user", "content": p}, asst_msg(r1), tool_result(tcs[0].id, big)]
        r2 = client.chat.completions.create(model=MODEL, user=mk, tools=[WEATHER], messages=msgs)
        check("bridge survives large result",
              r2.choices[0].finish_reason in ("stop", "length", "tool_calls"),
              f"finish={r2.choices[0].finish_reason}")
    except Exception as e:
        check("tool large", False, repr(e))


def t_tool_stream():
    print("\n== tool: 流式 + 协议字段 ==")
    try:
        mk = u("tool-stream")
        p = "必须调用 get_weather 查询上海天气。"
        stream = client.chat.completions.create(model=MODEL, user=mk, tools=[WEATHER], stream=True,
                                                messages=[{"role": "user", "content": p}])
        acc, finish = {}, None
        for c in stream:
            if not c.choices:
                continue
            d = c.choices[0].delta
            if d.tool_calls:
                for tc in d.tool_calls:
                    slot = acc.setdefault(tc.index, {"id": "", "name": "", "args": ""})
                    if tc.id:
                        slot["id"] = tc.id
                    if tc.function and tc.function.name:
                        slot["name"] = tc.function.name
                    if tc.function and tc.function.arguments:
                        slot["args"] += tc.function.arguments
            if c.choices[0].finish_reason:
                finish = c.choices[0].finish_reason
        check("stream tool_calls have index/id/name",
              len(acc) >= 1 and all(v["id"] and v["name"] for v in acc.values()),
              json.dumps(acc)[:160])
        check("stream finish_reason=tool_calls", finish == "tool_calls", str(finish))
        if not acc:
            return
        calls = [{"id": v["id"], "type": "function",
                  "function": {"name": v["name"], "arguments": v["args"] or "{}"}}
                 for _, v in sorted(acc.items())]
        msgs = [{"role": "user", "content": p},
                {"role": "assistant", "content": None, "tool_calls": calls}]
        for c in calls:
            msgs.append(tool_result(c["id"], "上海 多云 24℃"))
        r2 = client.chat.completions.create(model=MODEL, user=mk, tools=[WEATHER], messages=msgs)
        check("final after streamed tool_call", (r2.choices[0].message.content or "") != "",
              repr((r2.choices[0].message.content or "")[:50]))
    except Exception as e:
        check("tool stream", False, repr(e))


def t_tool_reasoning():
    print("\n== tool: 带 reasoning ==")
    try:
        mk = u("tool-reasoning")
        p = "必须调用 get_weather 查广州天气。"
        r1 = client.chat.completions.create(model=MODEL, user=mk, tools=[WEATHER], stream=True,
                                            messages=[{"role": "user", "content": p}])
        reasoning, got_tc = "", False
        for c in r1:
            if not c.choices:
                continue
            d = c.choices[0].delta
            if getattr(d, "reasoning_content", None):
                reasoning += d.reasoning_content
            if d.tool_calls:
                got_tc = True
        check("tool_call with reasoning stream ok", got_tc,
              f"reasoning={len(reasoning)}B tool_call={got_tc}")
    except Exception as e:
        check("tool reasoning", False, repr(e))


# ---------------- Responses 事件矩阵 ----------------

def sse_events(payload):
    req = urllib.request.Request(
        ROOT + "/v1/responses",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + KEY,
                 "Accept": "text/event-stream"})
    names, datas = [], []
    with urllib.request.urlopen(req, timeout=180) as resp:
        etype = None
        for raw in resp:
            line = raw.decode("utf-8").rstrip("\n")
            if line.startswith("event:"):
                etype = line[6:].strip()
            elif line.startswith("data:"):
                body = line[5:].strip()
                if body and body != "[DONE]":
                    try:
                        datas.append(json.loads(body))
                    except Exception:
                        datas.append({})
                    names.append(etype or "")
    return names, datas


def t_responses_events():
    print("\n== responses: SSE 事件序列 ==")
    try:
        names, datas = sse_events({"model": MODEL, "stream": True, "input": "只回两个字：你好"})
        check("response.created first", names and names[0] == "response.created", str(names[:3]))
        check("response.completed last", names and names[-1] == "response.completed", str(names[-3:]))
        check("has output_text.delta", "response.output_text.delta" in names,
              f"{names.count('response.output_text.delta')} deltas")
        done = datas[-1].get("response", {}) if datas else {}
        check("completed.status=completed", done.get("status") == "completed", str(done.get("status")))
        check("completed has output", bool(done.get("output")), f"{len(done.get('output') or [])} items")
    except Exception as e:
        check("responses events", False, repr(e))


def t_responses_function_call():
    print("\n== responses: function_call 事件 + 续链 ==")
    try:
        tools = [{"type": "function", "name": "get_weather", "description": "查天气",
                  "parameters": WEATHER["function"]["parameters"]}]
        names, datas = sse_events({"model": MODEL, "stream": True, "tools": tools,
                                   "input": "必须调用 get_weather 查询北京天气。"})
        check("output_item.added present", "response.output_item.added" in names,
              str([n for n in names if "output_item" in n][:3]))
        check("function_call_arguments.delta/done",
              "response.function_call_arguments.delta" in names and
              "response.function_call_arguments.done" in names,
              f"delta={'response.function_call_arguments.delta' in names} "
              f"done={'response.function_call_arguments.done' in names}")

        done = datas[-1].get("response", {}) if datas else {}
        fc = next((o for o in (done.get("output") or []) if o.get("type") == "function_call"), None)
        if not fc:
            check("function_call in output", False, json.dumps(done)[:200])
            return
        check("function_call has call_id/name/arguments",
              bool(fc.get("call_id") and fc.get("name") and fc.get("arguments") is not None),
              json.dumps(fc)[:160])

        req = urllib.request.Request(
            ROOT + "/v1/responses",
            data=json.dumps({"model": MODEL, "previous_response_id": done.get("id"), "stream": False,
                             "input": [{"type": "function_call_output", "call_id": fc["call_id"],
                                        "output": "北京 晴 26℃"}]}).encode(),
            headers={"Content-Type": "application/json", "Authorization": "Bearer " + KEY})
        with urllib.request.urlopen(req, timeout=180) as r:
            final = json.loads(r.read())
        out = final.get("output") or []
        has_text = (final.get("output_text") or "") != ""
        has_fc = any(o.get("type") == "function_call" for o in out)
        check("chain progressed", final.get("status") == "completed" and (has_text or has_fc),
              f"status={final.get('status')} text={has_text} fc={has_fc} raw={json.dumps(final)[:120]}")
        check("chain keeps previous_response_id", final.get("previous_response_id") == done.get("id"),
              str(final.get("previous_response_id")))
    except Exception as e:
        check("responses function_call", False, repr(e))


# ---------------- 并发 ----------------

def t_concurrency():
    print("\n== 并发：多客户端不串会话 ==")
    import concurrent.futures as cf

    def one(i):
        c = OpenAI(base_url=BASE, api_key=KEY, timeout=180.0, max_retries=0)
        tok = f"TOK{i}"
        r = c.chat.completions.create(
            model=MODEL, user=f"conc-{uuid.uuid4().hex[:8]}",
            messages=[{"role": "user", "content": f"只回这四个字符：{tok}"}])
        return i, tok, (r.choices[0].message.content or "")

    n = int(os.environ.get("MATRIX_CONCURRENCY", "8"))
    with cf.ThreadPoolExecutor(n) as ex:
        outs = list(ex.map(one, range(n)))
    bad = [i for i, tok, got in outs if tok not in got]
    check(f"{n} clients no cross-talk", not bad, f"mismatch={bad}")


# ---------------- 附件 ----------------

PNG_1PX = ("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8"
           "z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")


def t_attach_chat():
    print("\n== attach: Chat 图片 ==")
    try:
        mk = u("attach-chat")
        r = client.chat.completions.create(model=MODEL, user=mk, messages=[{
            "role": "user", "content": [
                {"type": "text", "text": "只回一个字：好"},
                {"type": "image_url", "image_url": {"url": "data:image/png;base64," + PNG_1PX}}]}])
        txt = r.choices[0].message.content or ""
        check("chat image accepted (200, 支持则附带/不支持则降级)", txt != "", repr(txt[:40]))
    except Exception as e:
        check("chat image", False, repr(e))


def t_attach_responses():
    print("\n== attach: Responses input_image ==")
    try:
        names, datas = sse_events({"model": MODEL, "stream": True, "input": [{
            "role": "user", "content": [
                {"type": "input_text", "text": "只回一个字：好"},
                {"type": "input_image", "image_url": "data:image/png;base64," + PNG_1PX}]}]})
        done = datas[-1].get("response", {}) if datas else {}
        check("responses image accepted (completed)", done.get("status") == "completed" and bool(done.get("output")),
              f"status={done.get('status')} out={len(done.get('output') or [])}")
    except Exception as e:
        check("responses image", False, repr(e))


TESTS = {
    "tool_single": t_tool_single,
    "tool_multi": t_tool_multi,
    "tool_error": t_tool_error,
    "tool_large": t_tool_large,
    "tool_stream": t_tool_stream,
    "tool_reasoning": t_tool_reasoning,
    "responses_events": t_responses_events,
    "responses_function_call": t_responses_function_call,
    "concurrency": t_concurrency,
    "attach_chat": t_attach_chat,
    "attach_responses": t_attach_responses,
}

GROUPS = {
    "tools": ["tool_single", "tool_multi", "tool_error", "tool_large", "tool_stream", "tool_reasoning"],
    "responses": ["responses_events", "responses_function_call"],
    "attach": ["attach_chat", "attach_responses"],
}

if __name__ == "__main__":
    args = sys.argv[1:] or list(TESTS)
    names = []
    for a in args:
        names.extend(GROUPS.get(a, [a]))
    print(f"bridge={BASE}  model={MODEL}")
    for n in names:
        if n not in TESTS:
            print(f"unknown test {n!r}; choose from {list(TESTS)} or {list(GROUPS)}")
            sys.exit(2)
        t0 = time.time()
        TESTS[n]()
        print(f"  ({time.time()-t0:.1f}s)")
    print(f"\n===== {len(PASS)} passed, {len(FAIL)} failed =====")
    if FAIL:
        print("failed:", ", ".join(FAIL))
    sys.exit(1 if FAIL else 0)
