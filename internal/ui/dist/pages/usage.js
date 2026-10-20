// pages/usage.js —— 本地用量记账：按渠道 / 凭据 / 模型 / 天聚合。
//
// 数据来自 /api/v1/usage/local（janus 自己从响应里解析并落库），与
// /api/v1/usage（上游 console 的全局账单）互补：多账号多渠道下，每份凭据、
// 每个 provider 各自独立统计，不依赖上游是否开放账单接口。

import { html } from '../html.js';
import { useMemo, useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty } from '../components.js';
import { fmtCompact, fmtDur, fmtInt, fmtPct, fmtTime, fmtUSD } from '../util.js';

const RANGES = [1, 7, 30, 90];

function qs(days, providerId, credentialId) {
  const p = new URLSearchParams();
  p.set('days', String(days));
  if (providerId) p.set('provider_id', providerId);
  if (credentialId) p.set('credential_id', credentialId);
  return p.toString();
}

function Bars({ days }) {
  const rows = days || [];
  if (rows.length === 0) return html`<${Empty}>该时间窗内没有用量<//>`;
  const W = 720, H = 170, padL = 44, padB = 26, padT = 12;
  const max = Math.max(1, ...rows.map((d) => (d.input_tokens || 0) + (d.output_tokens || 0)));
  const innerW = W - padL - 8, innerH = H - padB - padT;
  const bw = Math.max(2, Math.min(26, innerW / Math.max(1, rows.length) - 6));
  return html`<div style="overflow-x:auto">
    <svg viewBox=${`0 0 ${W} ${H}`} style="width:100%;min-width:520px;height:auto" role="img" aria-label="每日 token 趋势">
      <line x1=${padL} y1=${padT + innerH} x2=${W - 8} y2=${padT + innerH} stroke="var(--line)" />
      ${rows.map((d, i) => {
        const inTok = d.input_tokens || 0, outTok = d.output_tokens || 0;
        const total = inTok + outTok;
        const h = total > 0 ? Math.max(1, (total / max) * innerH) : 0;
        const x = padL + i * (innerW / rows.length) + 4;
        const hIn = total > 0 ? (inTok / total) * h : 0;
        const y = padT + innerH - h;
        return html`<g>
          <rect x=${x} y=${y} width=${bw} height=${h - hIn} fill="var(--acc)" rx="2"></rect>
          <rect x=${x} y=${y + h - hIn} width=${bw} height=${hIn} fill="var(--ok, #3a7)" rx="2"></rect>
          <title>${d.date} · 输入 ${fmtInt(inTok)} / 输出 ${fmtInt(outTok)}</title>
        </g>`;
      })}
    </svg>
  </div>`;
}

function RemoteItem({ it, credLabel }) {
  const name = credLabel[it.credential_id] || ((it.provider_id || '') + '/' + (it.label || it.credential_id || '–'));
  const when = it.fetched_at ? fmtTime(new Date(it.fetched_at).getTime()) : '';
  const t = it.totals || {};
  return html`<div class="card" style="margin-bottom:12px">
    <div class="cardhead">
      <b class="mono">${name}</b>
      <span class="badge">${it.auth_kind || '–'}</span>
      ${it.ok ? html`<span class="badge ok">在线</span>` : html`<span class="badge warn">失败</span>`}
      <span class="muted small">${it.source || ''}${when ? ' · ' + when : ''}</span>
    </div>
    ${!it.ok ? html`<div class="muted small">${it.error || '未取到远程数据'}</div>` : null}

    ${it.credits ? html`<div>
      <div class="grid c3" style="margin-bottom:8px">
        <div class="stat"><div class="k">剩余积分</div>
          <div class="v">${it.credits.unlimited ? '不限' : fmtCompact(it.credits.total || 0)}</div>
          <div class="d">${it.credits.region || ''}</div></div>
        <div class="stat"><div class="k">已用积分</div><div class="v">${fmtCompact(it.credits.total_used || 0)}</div>
          <div class="d">${it.credits.cycle_reset_time ? '周期重置 ' + it.credits.cycle_reset_time : ''}</div></div>
        <div class="stat"><div class="k">套餐</div><div class="v">${(it.credits.accounts || []).length}</div>
          <div class="d">${((it.credits.accounts || [])[0] || {}).package || ''}</div></div>
      </div>
      ${(it.credits.accounts || []).length ? html`<div class="tablewrap"><table>
        <thead><tr><th>套餐</th><th class="num">剩余</th><th class="num">已用</th>
          <th class="num">总量</th><th>到期</th></tr></thead>
        <tbody>${(it.credits.accounts || []).map((a, i) => html`<tr key=${i}>
          <td class="mono small">${a.package || '–'}</td>
          <td class="num">${a.unlimited ? '不限' : fmtCompact(a.remain || 0)}</td>
          <td class="num">${fmtCompact(a.used || 0)}</td>
          <td class="num">${a.size ? fmtCompact(a.size) : '–'}</td>
          <td class="small muted">${a.end_time_ms ? fmtTime(a.end_time_ms) : '–'}</td>
        </tr>`)}</tbody>
      </table></div>` : null}
    </div>` : null}

    ${it.billing ? html`<div>
      <div class="grid c4" style="margin-bottom:8px">
        <div class="stat"><div class="k">余额</div><div class="v">${fmtUSD(it.billing.balance_usd || 0)}</div>
          <div class="d">${it.billing.mode || ''}</div></div>
        <div class="stat"><div class="k">可用额度</div><div class="v">${fmtUSD(it.billing.available_usd || 0)}</div>
          <div class="d">${it.billing.billing_mode || ''}</div></div>
        <div class="stat"><div class="k">累计请求</div><div class="v">${fmtInt(t.requests || 0)}</div>
          <div class="d">花费 ${fmtUSD(t.cost_usd || 0)}</div></div>
        <div class="stat"><div class="k">累计 tokens</div>
          <div class="v">${fmtCompact((t.input_tokens || 0) + (t.output_tokens || 0))}</div>
          <div class="d">缓存读 ${fmtCompact(t.cache_read_tokens || 0)}</div></div>
      </div>
      ${it.go ? html`<div class="muted small">
        Go 套餐：5 小时 ${fmtPct(((it.go.rolling || {}).percent || 0) / 100)} ·
        周 ${fmtPct(((it.go.weekly || {}).percent || 0) / 100)} ·
        月 ${fmtPct(((it.go.monthly || {}).percent || 0) / 100)}
      </div>` : null}
    </div>` : null}
  </div>`;
}

export default function Usage() {
  const [days, setDays] = useState(7);
  const [providerId, setProviderId] = useState('');
  const [credentialId, setCredentialId] = useState('');
  const [remoteForce, setRemoteForce] = useState(0);

  const meta = useLoad(async () => {
    const [p, c] = await Promise.all([api.providers(), api.credentials()]);
    return { providers: p.providers || [], credentials: c.credentials || [] };
  }, []);

  const query = qs(days, providerId, credentialId);
  const usage = useLoad(() => api.usageLocal(query), [query]);
  const events = useLoad(() => api.usageEvents(query + '&limit=100'), [query]);

  const remoteQ = useMemo(() => {
    const p = new URLSearchParams();
    if (providerId) p.set('provider_id', providerId);
    if (credentialId) p.set('credential_id', credentialId);
    if (remoteForce) p.set('refresh', '1');
    return p.toString();
  }, [providerId, credentialId, remoteForce]);
  const remote = useLoad(() => api.usageRemote(remoteQ), [remoteQ]);
  const remoteItems = (remote.data && remote.data.items) || [];

  const providers = (meta.data && meta.data.providers) || [];
  const credentials = (meta.data && meta.data.credentials) || [];
  const credLabel = useMemo(() => {
    const m = {};
    for (const c of credentials) m[c.id] = (c.provider_id ? c.provider_id + '/' : '') + (c.label || c.id);
    return m;
  }, [credentials]);

  const summary = (usage.data && usage.data.summary) || {};
  const totals = summary.totals || {};
  const promptTotal = (totals.input_tokens || 0) + (totals.cache_read_tokens || 0);
  const rowEvents = (events.data && events.data.events) || [];

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>用量</h1>
        <div class="sub">
          本地记账（janus 从每次响应里解析 tokens / 缓存 / 成本），按渠道与凭据独立统计。
          ${usage.data && usage.data.fetched_at ? html` · 拉取于 ${fmtTime(usage.data.fetched_at)}` : null}
        </div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${() => { usage.reload(); events.reload(); }}>刷新</button>
      </div>
    </div>

    <div class="toolbar">
      <select value=${String(days)} onChange=${(e) => setDays(Number(e.target.value))}>
        ${RANGES.map((n) => html`<option value=${String(n)}>最近 ${n} 天</option>`)}
      </select>
      <select value=${providerId} onChange=${(e) => { setProviderId(e.target.value); setCredentialId(''); }}>
        <option value="">全部 provider</option>
        ${providers.map((p) => html`<option value=${p.id}>${p.id}</option>`)}
      </select>
      <select value=${credentialId} onChange=${(e) => setCredentialId(e.target.value)} disabled=${!providerId}>
        <option value="">全部凭据</option>
        ${credentials.filter((c) => !providerId || c.provider_id === providerId)
          .map((c) => html`<option value=${c.id}>${credLabel[c.id] || c.id}</option>`)}
      </select>
    </div>

    <${ErrorBox} error=${usage.error} onRetry=${usage.reload} />

    <div class="card" style="margin-bottom:12px">
      <div class="cardhead">
        <b>远程渠道用量</b>
        <span class="muted small">${remoteItems.length} 个凭据 · 各自上游账单 / 余额</span>
        <div class="actions">
          <button class="ghost" onClick=${() => setRemoteForce((n) => n + 1)} disabled=${remote.loading}>重拉</button>
        </div>
      </div>
      <${ErrorBox} error=${remote.error} onRetry=${remote.reload} />
      ${remote.loading ? html`<${Loading} />`
        : remoteItems.length === 0 ? html`<${Empty}>没有可查询的凭据（oauth / workbuddy 渠道才有远程账单）<//>`
        : html`<div>${remoteItems.map((it, i) => html`<${RemoteItem} key=${i} it=${it} credLabel=${credLabel} />`)}</div>`}
    </div>

    ${usage.loading ? html`<${Loading} />` : html`<div>
      <div class="grid c4" style="margin-bottom:12px">
        <div class="stat"><div class="k">请求数</div><div class="v">${fmtInt(totals.requests || 0)}</div>
          <div class="d">错误 ${fmtInt(totals.errors || 0)}</div></div>
        <div class="stat"><div class="k">输入 tokens</div><div class="v">${fmtCompact(totals.input_tokens || 0)}</div>
          <div class="d">缓存命中 ${fmtPct(promptTotal > 0 ? (totals.cache_read_tokens || 0) / promptTotal : 0)}</div></div>
        <div class="stat"><div class="k">输出 tokens</div><div class="v">${fmtCompact(totals.output_tokens || 0)}</div>
          <div class="d">推理 ${fmtCompact(totals.reasoning_tokens || 0)}</div></div>
        <div class="stat"><div class="k">预估花费</div><div class="v">${fmtUSD(totals.cost_usd || 0)}</div>
          <div class="d">平均耗时 ${totals.requests ? fmtDur(Math.round((totals.duration_ms || 0) / totals.requests / 1000)) : '–'}</div></div>
      </div>

      <div class="card" style="margin-bottom:12px">
        <div class="cardhead"><b>每日 token</b><span class="muted small">绿=输入（含缓存）· 蓝=输出</span></div>
        <${Bars} days=${summary.by_day} />
      </div>

      <div class="card" style="margin-bottom:12px">
        <div class="cardhead"><b>按渠道 / 凭据</b><span class="muted small">${(summary.by_credential || []).length} 个</span></div>
        ${(summary.by_credential || []).length === 0 ? html`<${Empty}>没有可聚合的凭据用量<//>`
          : html`<div class="tablewrap"><table>
              <thead><tr><th>渠道</th><th class="num">请求</th><th class="num">错误</th>
                <th class="num">输入</th><th class="num">缓存读</th><th class="num">输出</th>
                <th class="num">花费</th></tr></thead>
              <tbody>
                ${(summary.by_credential || []).map((b, i) => html`<tr key=${i}>
                  <td class="mono small">${credLabel[b.credential_id] || ((b.provider_id || '') + '/' + (b.credential_id || '–'))}</td>
                  <td class="num">${fmtInt(b.requests)}</td>
                  <td class="num">${b.errors ? html`<span class="badge warn">${fmtInt(b.errors)}</span>` : '0'}</td>
                  <td class="num">${fmtCompact(b.input_tokens)}</td>
                  <td class="num">${fmtCompact(b.cache_read_tokens)}</td>
                  <td class="num">${fmtCompact(b.output_tokens)}</td>
                  <td class="num">${fmtUSD(b.cost_usd)}</td>
                </tr>`)}
              </tbody>
            </table></div>`}
      </div>

      <div class="card" style="margin-bottom:12px">
        <div class="cardhead"><b>按模型</b><span class="muted small">${(summary.by_model || []).length} 个</span></div>
        ${(summary.by_model || []).length === 0 ? html`<${Empty}>没有可聚合的模型用量<//>`
          : html`<div class="tablewrap"><table>
              <thead><tr><th>模型</th><th class="num">请求</th><th class="num">输入</th>
                <th class="num">缓存读</th><th class="num">输出</th><th class="num">花费</th></tr></thead>
              <tbody>
                ${(summary.by_model || []).map((b, i) => html`<tr key=${i}>
                  <td class="mono small">${b.model || '–'}</td>
                  <td class="num">${fmtInt(b.requests)}</td>
                  <td class="num">${fmtCompact(b.input_tokens)}</td>
                  <td class="num">${fmtCompact(b.cache_read_tokens)}</td>
                  <td class="num">${fmtCompact(b.output_tokens)}</td>
                  <td class="num">${fmtUSD(b.cost_usd)}</td>
                </tr>`)}
              </tbody>
            </table></div>`}
      </div>

      <div class="card">
        <div class="cardhead"><b>最近请求</b><span class="muted small">最多 100 条</span></div>
        <${ErrorBox} error=${events.error} onRetry=${events.reload} />
        ${events.loading ? html`<${Loading} />`
          : rowEvents.length === 0 ? html`<${Empty}>还没有本地记账（仅直连模型面会记账）<//>`
          : html`<div class="tablewrap"><table>
              <thead><tr><th>时间</th><th>渠道</th><th>模型</th><th class="num">状态</th>
                <th class="num">输入</th><th class="num">缓存读</th><th class="num">输出</th>
                <th class="num">耗时</th><th class="num">花费</th></tr></thead>
              <tbody>
                ${rowEvents.map((e, i) => html`<tr key=${i}>
                  <td class="small muted">${fmtTime(e.ts * 1000)}</td>
                  <td class="mono small">${credLabel[e.credential_id] || (e.provider_id || '–')}</td>
                  <td class="mono small">${e.upstream_model || e.model || '–'}</td>
                  <td class="num">${e.status >= 200 && e.status < 400
                    ? html`<span class="badge ok">${e.status}</span>`
                    : html`<span class="badge warn">${e.status || 'ERR'}</span>`}</td>
                  <td class="num">${fmtCompact(e.input_tokens)}</td>
                  <td class="num">${fmtCompact(e.cache_read_tokens)}</td>
                  <td class="num">${fmtCompact(e.output_tokens)}</td>
                  <td class="num small">${e.duration_ms ? fmtDur(Math.round(e.duration_ms / 1000)) : '–'}</td>
                  <td class="num">${fmtUSD(e.cost_usd)}</td>
                </tr>`)}
              </tbody>
            </table></div>`}
      </div>
    </div>`}
  </div>`;
}
