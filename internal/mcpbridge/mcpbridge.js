// janus MCP stdio bridge —— 把 opencode 的 stdio MCP 传输代理到 janus 的 HTTP 端点。
//
// 为什么需要：Bun（opencode 的运行时）的 fetch 有 ~360 秒的总时长硬顶，无视
// 调用方的 AbortSignal、也无视数据是否在流动（均已实测），导致 remote MCP 的
// 长工具调用（编译/测试/发布类命令，客户端执行动辄数分钟）必然被掐死。本脚本
// 以 local(stdio) 方式被 opencode 拉起，自己的 fetch 显式传非标准的 timeout
// 选项覆盖默认值（Bun 1.4.2 实证生效），把单次调用的存活期延长到 janus 的
// ToolCallWait（30m）以上。
//
// 协议：stdin/stdout 收发换行分隔的 JSON-RPC（MCP stdio 规范），每个消息原样
// POST 到 JANUS_MCP_URL（janus 的 /mcp/<token> 端点），响应原样写回 stdout。
// janus 端点对每个 POST 回单个 JSON（202/204 表示通知类确认），无需 SSE 解析。
//
// 环境变量（由 janus 注册时注入）：
//   JANUS_MCP_URL            janus 的 /mcp/<token> 端点（必填）
//   JANUS_MCP_FETCH_TIMEOUT  单次转发请求的存活期 ms（默认 3600000 = 1h，
//                            需大于 janus 的 ToolCallWait，让 janus 的超时先到）
const url = process.env.JANUS_MCP_URL || "";
const timeout = Number(process.env.JANUS_MCP_FETCH_TIMEOUT || 3600000);
if (!url) {
  process.stderr.write("janus-mcp-bridge: JANUS_MCP_URL is not set\n");
  process.exit(1);
}

let buf = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  buf += chunk;
  let i;
  while ((i = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, i).trim();
    buf = buf.slice(i + 1);
    if (line) forward(line);
  }
});
process.stdin.on("end", () => process.exit(0));
process.on("uncaughtException", (e) => {
  process.stderr.write("janus-mcp-bridge: " + ((e && e.message) || String(e)) + "\n");
});

async function forward(line) {
  let msg = null;
  try {
    msg = JSON.parse(line);
  } catch {
    return;
  }
  const hasId = msg && msg.id !== undefined && msg.id !== null;
  try {
    const res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream" },
      body: line,
      // 覆盖 Bun fetch 默认 ~360s 总时长硬顶（非标准选项，Bun 1.4 实证生效）
      timeout,
      signal: AbortSignal.timeout(timeout),
    });
    if (res.status === 202 || res.status === 204) return; // 通知类确认，无回包
    const body = await res.text();
    if (!hasId || !body.trim()) return;
    process.stdout.write(body.replace(/\s+$/, "") + "\n");
  } catch (e) {
    if (!hasId) return;
    const err = {
      jsonrpc: "2.0",
      id: msg.id,
      error: { code: -32000, message: "bridge: upstream request failed: " + ((e && e.message) || String(e)) },
    };
    process.stdout.write(JSON.stringify(err) + "\n");
  }
}
