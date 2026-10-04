#!/usr/bin/env python3
"""
compat_test.py — 用 OpenAI 官方 SDK 打 bridge，验证 Trae/CodeBuddy 这类客户端的兼容面。

用法：
  python3 compat_test.py            # 全部
  python3 compat_test.py models     # 只跑某一组
"""
import json
import os
import sys
import time

from openai import OpenAI

BASE = os.environ.get("BRIDGE_URL", "http://127.0.0.1:2810/v1")
KEY = os.environ.get("BRIDGE_API_KEY", "sk-bridge-dev")
MODEL = os.environ.get("BRIDGE_MODEL", "opencode/fledge-alpha-free")

client = OpenAI(base_url=BASE, api_key=KEY, timeout=180.0, max_retries=0)

PASS, FAIL = [], []


def check(name, ok, detail=""):
    (PASS if ok else FAIL).append(name)
    print(f"  [{'PASS' if ok else 'FAIL'}] {name}" + (f" — {detail}" if detail else ""))


def t_models():
    print("\n== /v1/models ==")
    try:
        lst = client.models.list()
        check("models.list()", lst.object == "list" and len(lst.data) > 0,
              f"{len(lst.data)} models")
        ids = [m.id for m in lst.data]
        check("ids are provider/model form", any("/" in i for i in ids), ids[0])
        one = client.models.retrieve(ids[0])
        check("models.retrieve()", one.id == ids[0])
    except Exception as e:
        check("models api", False, repr(e))


def t_nostream():
    print("\n== chat 非流式 ==")
    try:
        r = client.chat.completions.create(
            model=MODEL, messages=[{"role": "user", "content": "回答：1+1=? 只答数字"}])
        check("has choices", len(r.choices) == 1)
        ch = r.choices[0]
        check("role=assistant", ch.message.role == "assistant")
        check("content is str", isinstance(ch.message.content, str) and ch.message.content != "",
              repr(ch.message.content[:60]))
        check("finish_reason set", ch.finish_reason in
              ("stop", "length", "tool_calls", "content_filter"), str(ch.finish_reason))
        check("usage present", r.usage is not None and r.usage.total_tokens > 0,
              str(r.usage and r.usage.total_tokens))
        check("id/object/model", r.id.startswith("chatcmpl-") and
              r.object == "chat.completion" and r.model == MODEL, f"{r.id} {r.model}")
    except Exception as e:
        check("nostream", False, repr(e))


def t_stream():
    print("\n== chat 流式 ==")
    try:
        s = client.chat.completions.create(
            model=MODEL, stream=True,
            messages=[{"role": "user", "content": "从1数到4，每行一个数字"}])
        text, roles, finishes, chunks = "", [], [], 0
        for c in s:
            chunks += 1
            if not c.choices:
                continue
            d = c.choices[0].delta
            if d.role:
                roles.append(d.role)
            text += d.content or ""
            if c.choices[0].finish_reason:
                finishes.append(c.choices[0].finish_reason)
        check("streamed chunks", chunks > 2, f"{chunks} chunks")
        check("first chunk has role", "assistant" in roles, str(roles[:3]))
        check("content accumulated", len(text) > 3, repr(text[:80]))
        check("finish_reason in stream", len(finishes) == 1 and finishes[0] == "stop",
              str(finishes))
    except Exception as e:
        check("stream", False, repr(e))


def t_stream_options_usage():
    print("\n== stream_options.include_usage ==")
    try:
        s = client.chat.completions.create(
            model=MODEL, stream=True, stream_options={"include_usage": True},
            messages=[{"role": "user", "content": "回答：天空什么颜色？只答颜色"}])
        usage, tail = None, 0
        for c in s:
            tail += 1
            if c.usage:
                usage = c.usage
        check("usage chunk present", usage is not None)
        check("usage totals sane",
              usage is not None and usage.total_tokens ==
              usage.prompt_tokens + usage.completion_tokens,
              f"p={usage and usage.prompt_tokens} c={usage and usage.completion_tokens}")
    except Exception as e:
        check("stream_options", False, repr(e))


def t_multiturn():
    print("\n== 多轮（history 复用同一 session）==")
    try:
        msgs = [{"role": "user", "content": "记住：我的代号是 Kestrel。只答“已记”"}]
        r1 = client.chat.completions.create(model=MODEL, messages=msgs)
        msgs.append({"role": "assistant", "content": r1.choices[0].message.content})
        msgs.append({"role": "user", "content": "我的代号是什么？只答代号"})
        r2 = client.chat.completions.create(model=MODEL, messages=msgs)
        got = r2.choices[0].message.content
        check("context retained", "Kestrel" in (got or ""), repr((got or "")[:60]))
    except Exception as e:
        check("multiturn", False, repr(e))


def t_reasoning():
    print("\n== reasoning_content 字段 ==")
    try:
        r = client.chat.completions.create(
            model=MODEL, messages=[{"role": "user", "content": "回答：地球绕什么转？只答天体"}])
        msg = r.choices[0].message
        check("content unaffected by reasoning", isinstance(msg.content, str) and msg.content != "",
              repr(msg.content[:50]))
        rc = getattr(msg, "reasoning_content", None)
        print(f"       reasoning_content: {type(rc).__name__} "
              f"{len(rc) if isinstance(rc, str) else '-'} chars")
    except Exception as e:
        check("reasoning", False, repr(e))


def t_errors():
    print("\n== 错误语义 ==")
    try:
        client.chat.completions.create(model="definitely/not-a-model",
                                       messages=[{"role": "user", "content": "x"}])
        check("unknown model raises", False, "no exception")
    except Exception as e:
        code = getattr(e, "status_code", None)
        check("unknown model -> 404 NotFoundError", code == 404, f"{type(e).__name__} {code}")

    try:
        client.chat.completions.create(model=MODEL, messages=[])
        check("empty messages raises", False, "no exception")
    except Exception as e:
        code = getattr(e, "status_code", None)
        check("empty messages -> 400", code == 400, f"{type(e).__name__} {code}")

    bad = OpenAI(base_url=BASE, api_key="sk-wrong", timeout=30.0, max_retries=0)
    try:
        bad.models.list()
        check("bad key raises", False, "no exception")
    except Exception as e:
        code = getattr(e, "status_code", None)
        check("bad key -> 401", code == 401, f"{type(e).__name__} {code}")


def t_image():
    print("\n== 图片消息（content 数组形态）==")
    # 1x1 红色 PNG
    png = ("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8"
           "z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
    try:
        r = client.chat.completions.create(
            model=MODEL, messages=[{
                "role": "user",
                "content": [
                    {"type": "text", "text": "这张图是什么颜色？只答颜色"},
                    {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{png}"}},
                ],
            }])
        check("image request succeeds",
              isinstance(r.choices[0].message.content, str),
              repr((r.choices[0].message.content or "")[:60]))
    except Exception as e:
        # 图片失败不应拖垮整个请求，记录但不算硬失败
        check("image request succeeds", False, repr(e))


def t_concurrency():
    print("\n== 并发会话隔离 ==")
    import concurrent.futures as cf

    def one(tag, prompt):
        c = OpenAI(base_url=BASE, api_key=KEY, timeout=180.0, max_retries=0)
        r = c.chat.completions.create(
            model=MODEL, messages=[{"role": "user", "content": prompt}])
        return tag, r.choices[0].message.content or ""

    jobs = [("A", "从1数到3，每行一个数字", ["1", "2", "3"]),
            ("B", "从5数到7，每行一个数字", ["5", "6", "7"]),
            ("C", "从8数到9，每行一个数字", ["8", "9"])]

    def run(job):
        tag, prompt, _ = job
        return one(tag, prompt)

    with cf.ThreadPoolExecutor(3) as ex:
        out = dict(ex.map(run, jobs))

    ok = True
    for tag, _, want in jobs:
        got = out.get(tag, "")
        if not all(w in got for w in want):
            ok = False
    check("no cross-talk", ok, json.dumps(out, ensure_ascii=False)[:200])


def t_reasoning_effort():
    print("\n== reasoning_effort（思考强度）==")
    for effort in ("low", "max", "medium", "minimal"):
        try:
            r = client.chat.completions.create(
                model=MODEL, reasoning_effort=effort,
                messages=[{"role": "user", "content": "只答：ok"}])
            txt = r.choices[0].message.content or ""
            check(f"reasoning_effort={effort}", r.model == MODEL and txt != "",
                  f"model={r.model}")
        except Exception as e:
            check(f"reasoning_effort={effort}", False, repr(e))

    # 未知档位名不应报错，桥会忽略
    try:
        r = client.chat.completions.create(
            model=MODEL, reasoning_effort="supersonic",
            messages=[{"role": "user", "content": "只答：ok"}])
        check("unknown effort ignored", r.choices[0].message.content != "")
    except Exception as e:
        check("unknown effort ignored", False, repr(e))


def t_models_extensions():
    print("\n== /v1/models 扩展字段 ==")
    try:
        lst = client.models.list()
        real = [m for m in lst.data if m.id != "default"]
        with_ctx = [m for m in real if getattr(m, "context_length", None)]
        check("context_length present", len(with_ctx) == len(real),
              f"{len(with_ctx)}/{len(real)}")
        with_out = [m for m in real if getattr(m, "max_output_tokens", None)]
        check("max_output_tokens present", len(with_out) == len(real),
              f"{len(with_out)}/{len(real)}")
        with_eff = [m for m in real if getattr(m, "supported_reasoning_efforts", None)]
        check("supported_reasoning_efforts present", len(with_eff) > 0,
              f"{len(with_eff)}/{len(real)}")
        sample = next(m for m in real if getattr(m, "supported_reasoning_efforts", None))
        print(f"       e.g. {sample.id} ctx={sample.context_length} "
              f"out={sample.max_output_tokens} "
              f"efforts={sample.supported_reasoning_efforts}")
    except Exception as e:
        check("models extensions", False, repr(e))


TESTS = {
    "models": t_models,
    "nostream": t_nostream,
    "stream": t_stream,
    "usage": t_stream_options_usage,
    "multiturn": t_multiturn,
    "reasoning": t_reasoning,
    "errors": t_errors,
    "image": t_image,
    "concurrency": t_concurrency,
    "effort": t_reasoning_effort,
    "modelsext": t_models_extensions,
}

if __name__ == "__main__":
    names = sys.argv[1:] or list(TESTS)
    for n in names:
        if n not in TESTS:
            print(f"unknown test {n!r}; choose from {list(TESTS)}")
            sys.exit(2)
        t0 = time.time()
        TESTS[n]()
        print(f"  ({time.time()-t0:.1f}s)")
    print(f"\n===== {len(PASS)} passed, {len(FAIL)} failed =====")
    if FAIL:
        print("failed:", ", ".join(FAIL))
    sys.exit(1 if FAIL else 0)
