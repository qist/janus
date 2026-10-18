// pages/requests.js —— 上游请求日志：逐条 token / 缓存 / 耗时 / 花费。
//
// 数据来自 /api/v1/requests（后端转发上游 console 的 request-logs，带 5s 缓存），
// 所以这里不做分页，只提供条数上限与本地过滤。

import { html } from '../html.js';
import { useMemo, useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty } from '../components.js';
import { fmtCompact, fmtDur, fmtInt, fmtPct, fmtTime, fmtUSD } from '../util.js';

const LIMITS = [50, 100, 200];

// finish 原因 → 徽章颜色。正常收尾用 ok，工具调用用 acc，其余（长度截断、
// 内容过滤、错误）用 warn，避免把非正常结束误当成成功。
function finishKind(finish) {
  const f = String(finish || '').toLowerCase();
  if (!f) return '';
  if (f === 'stop' || f === 'end_turn' || f === 'stop_sequence') return 'ok';
  if (f.includes('tool')) return 'acc';
  return 'warn';
}

export default function Requests() {
  const [limit, setLimit] = useState(100);
  const [q, setQ] = useState('');
  const list = useLoad(() => api.requests(limit), [limit]);

  const all = (list.data && list.data.requests) || [];
  const rows = useMemo(() => {
    const kw = q.trim().toLowerCase();
    if (!kw) return all;
    return all.filter((r) =>
      [r.model, r.provider, r.session, r.finish].some((v) => String(v || '').toLowerCase().includes(kw)));
  }, [all, q]);

  const sum = useMemo(() => rows.reduce((acc, r) => {
    acc.input += r.input_tokens || 0;
    acc.cache += r.cache_read_tokens || 0;
    acc.output += r.output_tokens || 0;
    acc.cost += r.cost_usd || 0;
    acc.ms += r.duration_ms || 0;
    return acc;
  }, { input: 0, cache: 0, output: 0, cost: 0, ms: 0 }), [rows]);

  const promptTotal = sum.input + sum.cache;

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>请求日志</h1>
        <div class="sub">
          上游 console 的逐条记录（含缓存命中率），用于对账；最多 200 条。
          ${list.data && list.data.fetched_at ? html` · 拉取于 ${fmtTime(list.data.fetched_at)}` : null}
        </div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${list.reload}>刷新</button>
      </div>
    </div>

    <div class="toolbar">
      <input value=${q} onInput=${(e) => setQ(e.target.value)} placeholder="过滤 model / provider / session" spellcheck="false" />
      <select value=${String(limit)} onChange=${(e) => setLimit(Number(e.target.value))}>
        ${LIMITS.map((n) => html`<option value=${String(n)}>最近 ${n} 条</option>`)}
      </select>
      <span class="muted small">${rows.length} 条${q ? '（已过滤）' : ''}</span>
    </div>

    <${ErrorBox} error=${list.error} onRetry=${list.reload} />

    ${list.loading ? html`<${Loading} />`
      : rows.length === 0 ? html`<div class="card"><${Empty}>
          ${q ? '没有匹配的记录' : '还没有请求记录（或未配置 console_url / opencode_db）'}
        <//></div>`
      : html`<div>
          <div class="grid c4" style="margin-bottom:12px">
            <div class="stat"><div class="k">请求数</div><div class="v">${fmtInt(rows.length)}</div></div>
            <div class="stat"><div class="k">输入 tokens</div><div class="v">${fmtCompact(sum.input)}</div>
              <div class="d">缓存命中 ${fmtPct(promptTotal > 0 ? sum.cache / promptTotal : 0)}</div></div>
            <div class="stat"><div class="k">输出 tokens</div><div class="v">${fmtCompact(sum.output)}</div></div>
            <div class="stat"><div class="k">花费</div><div class="v">${fmtUSD(sum.cost)}</div>
              <div class="d">平均耗时 ${rows.length ? fmtDur(Math.round(sum.ms / rows.length / 1000)) : '–'}</div></div>
          </div>

          <div class="tablewrap"><table>
            <thead><tr>
              <th>时间</th><th>模型</th><th>Provider</th><th>会话</th>
              <th class="num">输入</th><th class="num">缓存读</th><th class="num">缓存写</th>
              <th class="num">命中率</th><th class="num">输出</th><th class="num">推理</th>
              <th class="num">耗时</th><th class="num">花费</th><th>结束</th>
            </tr></thead>
            <tbody>
              ${rows.map((r, i) => html`<tr key=${i}>
                <td class="small muted">${fmtTime(r.time)}</td>
                <td class="mono">${r.model || '–'}</td>
                <td class="small">${r.provider || '–'}</td>
                <td class="mono small">${r.session || html`<span class="muted">–</span>`}</td>
                <td class="num">${fmtCompact(r.input_tokens)}</td>
                <td class="num">${fmtCompact(r.cache_read_tokens)}</td>
                <td class="num">${fmtCompact(r.cache_write_tokens)}</td>
                <td class="num">${fmtPct(r.cache_hit_rate)}</td>
                <td class="num">${fmtCompact(r.output_tokens)}</td>
                <td class="num">${r.reasoning_tokens ? fmtCompact(r.reasoning_tokens) : html`<span class="muted">–</span>`}</td>
                <td class="num small">${r.duration_ms ? fmtDur(Math.round(r.duration_ms / 1000)) : '–'}</td>
                <td class="num">${fmtUSD(r.cost_usd)}</td>
                <td><span class=${'badge ' + finishKind(r.finish)}>${r.finish || '–'}</span></td>
              </tr>`)}
            </tbody>
          </table></div>
        </div>`}
  </div>`;
}
