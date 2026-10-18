// pages/overview.js —— 概览：健康状态、累计用量、每日趋势、模型分布。

import { html } from '../html.js';
import { useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty, toast } from '../components.js';
import { fmtInt, fmtCompact, fmtUSD, fmtPct, fmtTime, shortDate } from '../util.js';

function Stat({ k, v, d, onClick }) {
  return html`<div class=${'stat' + (onClick ? ' clickable' : '')} onClick=${onClick}>
    <div class="k">${k}</div>
    <div class="v">${v}</div>
    ${d ? html`<div class="d">${d}</div>` : null}
  </div>`;
}

function Chart({ days }) {
  const W = 720, H = 190, padL = 44, padB = 26, padT = 12;
  const max = Math.max(1, ...days.map((d) => d.tokens || 0));
  const innerW = W - padL - 8;
  const innerH = H - padB - padT;
  const bw = Math.max(2, Math.min(26, innerW / Math.max(1, days.length) - 6));

  return html`<div style="overflow-x:auto">
    <svg viewBox=${`0 0 ${W} ${H}`} style="width:100%;min-width:520px;height:auto" role="img" aria-label="每日 token 趋势">
      <defs>
        <linearGradient id="gbar" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="var(--accent)" stop-opacity=".95" />
          <stop offset="100%" stop-color="var(--accent)" stop-opacity=".35" />
        </linearGradient>
      </defs>
      ${[0, 0.25, 0.5, 0.75, 1].map((f) => {
        const y = padT + innerH * (1 - f);
        return html`<g>
          <line x1=${padL} y1=${y} x2=${W - 8} y2=${y} stroke="var(--line)" stroke-width="1" />
          <text x=${padL - 8} y=${y + 4} text-anchor="end" font-size="10" fill="var(--muted)">${fmtCompact(max * f)}</text>
        </g>`;
      })}
      ${days.map((d, i) => {
        const h = innerH * ((d.tokens || 0) / max);
        const x = padL + (innerW / days.length) * i + (innerW / days.length - bw) / 2;
        return html`<g>
          <rect x=${x} y=${padT + innerH - h} width=${bw} height=${Math.max(h, 1)} rx="2" fill="url(#gbar)">
            <title>${d.date}：${fmtInt(d.tokens)} tokens / ${fmtInt(d.requests)} 请求 / ${fmtUSD(d.cost_usd, true)}</title>
          </rect>
          ${days.length <= 16 || i % Math.ceil(days.length / 12) === 0
            ? html`<text x=${x + bw / 2} y=${H - 8} text-anchor="middle" font-size="10" fill="var(--muted)">${shortDate(d.date)}</text>`
            : null}
        </g>`;
      })}
    </svg>
  </div>`;
}

export default function Overview() {
  const usage = useLoad(() => api.usage(), []);
  const reqs = useLoad(() => api.requests(20), []);

  const rep = usage.data;
  const totals = (rep && rep.totals) || {};
  const billing = (rep && rep.billing) || {};
  const logs = (reqs.data && reqs.data.requests) || [];
  const promptTotal = (totals.input_tokens || 0) + (totals.cache_read_tokens || 0);
  const hitRate = promptTotal > 0 ? (totals.cache_read_tokens || 0) / promptTotal : 0;

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>概览</h1>
        <div class="sub">
          上游用量与请求情况
          ${rep && rep.fetched_at ? html` · 数据时间 ${fmtTime(rep.fetched_at)}` : null}
        </div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${() => { usage.reload(); reqs.reload(); }}>刷新</button>
      </div>
    </div>

    ${usage.error ? html`<div class="alert">
      用量接口不可用（可能未配置 <code>opencode_db</code> / <code>console_url</code>）：
      <span class="muted">${usage.error.message}</span>
    </div>` : null}

    <div class="grid c4">
      <${Stat} k="累计请求" v=${fmtInt(totals.requests)} d="上游 console 统计" />
      <${Stat} k="输入 tokens" v=${fmtCompact(totals.input_tokens)} d=${'缓存命中 ' + fmtPct(hitRate)} />
      <${Stat} k="输出 tokens" v=${fmtCompact(totals.output_tokens)} />
      <${Stat} k="累计花费" v=${fmtUSD(totals.cost_usd)} />
    </div>

    ${billing && (billing.balance_usd !== undefined) ? html`<div class="grid c4" style="margin-top:12px">
      <${Stat} k="余额" v=${fmtUSD(billing.balance_usd, true)} d=${billing.mode || ''} />
      <${Stat} k="可用额度" v=${fmtUSD(billing.available_usd, true)} d=${billing.billing_mode || ''} />
      <${Stat} k="缓存读取 tokens" v=${fmtCompact(totals.cache_read_tokens)} d="cache_read_tokens" />
      <${Stat} k="缓存写入 tokens" v=${fmtCompact((totals.cache_write_5m_tokens || 0) + (totals.cache_write_1h_tokens || 0))} d="5m + 1h" />
    </div>` : null}

    <div class="card" style="margin-top:14px">
      <div class="cardhead">
        <h2>每日趋势</h2>
        <span class="badge">tokens</span>
      </div>
      ${usage.loading ? html`<${Loading} />`
        : (rep && rep.by_day && rep.by_day.length) ? html`<${Chart} days=${rep.by_day} />`
        : html`<${Empty}>暂无每日数据<//>`}
    </div>

    <div class="split" style="margin-top:14px">
      <div class="card">
        <div class="cardhead"><h2>最近请求</h2>
          <div class="actions"><a class="small" href="#/requests">全部 →</a></div>
        </div>
        ${reqs.loading ? html`<${Loading} />`
          : reqs.error ? html`<div class="alert">请求日志不可用：${reqs.error.message}</div>`
          : logs.length === 0 ? html`<${Empty}>还没有请求记录<//>`
          : html`<div class="tablewrap"><table>
              <thead><tr><th>时间</th><th>模型</th><th class="num">输入</th><th class="num">命中</th><th class="num">输出</th><th>状态</th></tr></thead>
              <tbody>
                ${logs.slice(0, 12).map((r) => html`<tr>
                  <td class="small muted">${fmtTime(r.time)}</td>
                  <td class="mono">${r.model}</td>
                  <td class="num">${fmtCompact(r.input_tokens)}</td>
                  <td class="num">${fmtPct(r.cache_hit_rate)}</td>
                  <td class="num">${fmtCompact(r.output_tokens)}</td>
                  <td><span class=${'badge ' + (r.finish === 'stop' || r.finish === 'tool-calls' ? 'ok' : 'warn')}>${r.finish || '–'}</span></td>
                </tr>`)}
              </tbody>
            </table></div>`}
      </div>

      <div class="card">
        <div class="cardhead"><h2>模型用量</h2></div>
        ${usage.loading ? html`<${Loading} />`
          : (rep && rep.models && rep.models.length) ? html`<div class="tablewrap"><table>
              <thead><tr><th>模型</th><th class="num">请求</th><th class="num">花费</th></tr></thead>
              <tbody>
                ${rep.models.slice(0, 10).map((m) => html`<tr>
                  <td class="mono wrap">${m.provider ? m.provider + '/' : ''}${m.model}</td>
                  <td class="num">${fmtInt(m.requests)}</td>
                  <td class="num">${fmtUSD(m.cost_usd)}</td>
                </tr>`)}
              </tbody>
            </table></div>` : html`<${Empty}>暂无模型用量<//>`}
      </div>
    </div>
  </div>`;
}
