#!/usr/bin/env python3
"""Janus 并发压测：验证高并发下不串会话、桥内存/会话数可控。

用法（需先起 Janus 且装 openai）：
    BRIDGE_MODEL=opencode-go/deepseek-v4.1-flash \
    N=100 .venv/bin/python tests/stress.py

环境变量：
    BRIDGE_BASE   默认 http://127.0.0.1:2810/v1（OpenAI SDK 会拼 /chat/completions）
    BRIDGE_API_KEY 默认 sk-bridge-dev
    BRIDGE_MODEL  必须是一个可用的模型（免费档经 API 会 403）
    N             并发数，默认 100
    MAX_TOKENS    每次输出上限，默认 32（压低成本/时长）
    JANUS_PID     可选，桥进程 pid；给了就采样其 RSS
"""
import concurrent.futures as cf
import os
import time
import uuid

from openai import OpenAI

BASE = os.environ.get("BRIDGE_BASE", "http://127.0.0.1:2810/v1")
KEY = os.environ.get("BRIDGE_API_KEY", "sk-bridge-dev")
MODEL = os.environ.get("BRIDGE_MODEL", "")
N = int(os.environ.get("N", "100"))
MAX_TOKENS = int(os.environ.get("MAX_TOKENS", "32"))
PID = os.environ.get("JANUS_PID", "")

if not MODEL:
    raise SystemExit("请设置 BRIDGE_MODEL（可用模型，如 opencode-go/deepseek-v4.1-flash）")


def rss_kb():
    try:
        with open(f"/proc/{PID}/status") as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1])
    except Exception:
        pass
    return None


def one(i):
    c = OpenAI(base_url=BASE, api_key=KEY, timeout=300.0, max_retries=0)
    tok = f"T{i}"
    t0 = time.time()
    try:
        r = c.chat.completions.create(
            model=MODEL, user=f"stress-{uuid.uuid4().hex[:10]}",
            max_tokens=MAX_TOKENS,
            messages=[{"role": "user", "content": f"只回这几个字符，不要多余内容：{tok}"}])
        got = r.choices[0].message.content or ""
        return {"i": i, "ok": True, "cross": tok not in got, "got": got[:80], "dt": time.time() - t0,
                "pt": (r.usage.prompt_tokens if r.usage else 0),
                "ct": (r.usage.completion_tokens if r.usage else 0)}
    except Exception as e:  # noqa: BLE001
        return {"i": i, "ok": False, "err": repr(e)[:160], "dt": time.time() - t0}


def main():
    before = rss_kb()
    t0 = time.time()
    with cf.ThreadPoolExecutor(N) as ex:
        outs = list(ex.map(one, range(N)))
    wall = time.time() - t0
    after = rss_kb()

    ok = [o for o in outs if o.get("ok")]
    failed = [o for o in outs if not o.get("ok")]
    cross = [o["i"] for o in ok if o.get("cross")]
    dts = sorted(o["dt"] for o in ok)

    print(f"并发={N} 模型={MODEL} max_tokens={MAX_TOKENS}")
    print(f"成功={len(ok)}/{N}  串会话={len(cross)}  失败={len(failed)}  总耗时={wall:.1f}s")
    if dts:
        print(f"延迟 p50={dts[len(dts)//2]:.2f}s p95={dts[int(len(dts)*0.95)-1]:.2f}s max={dts[-1]:.2f}s")
    if before and after:
        print(f"Janus RSS: {before/1024:.1f}MB -> {after/1024:.1f}MB (+{(after-before)/1024:.1f}MB)")
    for o in failed[:8]:
        print("  FAIL", o["i"], o["err"])
    for o in [x for x in ok if x.get("cross")][:5]:
        print("  CROSS", o["i"], "got:", repr(o.get("got")))
    if cross:
        print("  CROSS-TALK at", cross[:8])

    raise SystemExit(1 if (failed or cross) else 0)


if __name__ == "__main__":
    main()
