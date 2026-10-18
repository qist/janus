// pages/sessions.js —— 会话表（conversations）。
//
// janus 为每个上游会话持久化一条记录：key 是内部主键，session_id 是上游会话，
// 其余字段是上次请求命中的 provider / 模型 / 凭据。删除会同时清掉上游会话。

import { html } from '../html.js';
import { useMemo, useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty, toast, toastErr, confirmDialog } from '../components.js';
import { fmtUnix } from '../util.js';

export default function Sessions() {
  const list = useLoad(() => api.sessions(), []);
  const [q, setQ] = useState('');
  const [busyKey, setBusyKey] = useState('');

  const all = (list.data && list.data.sessions) || [];
  const rows = useMemo(() => {
    const kw = q.trim().toLowerCase();
    if (!kw) return all;
    return all.filter((s) =>
      [s.key, s.session_id, s.provider_id, s.model_id, s.agent, s.agent_id, s.directory]
        .some((v) => String(v || '').toLowerCase().includes(kw)));
  }, [all, q]);

  const del = async (s) => {
    const ok = await confirmDialog({
      title: '删除会话',
      message: html`删除 <b class="mono">${s.key}</b>？
        ${s.session_id ? html`<div class="small muted" style="margin-top:6px">
          同时会删除上游会话 <span class="mono">${s.session_id}</span>，历史前缀缓存随之失效。</div>` : null}`,
      danger: true, okText: '删除',
    });
    if (!ok) return;
    setBusyKey(s.key);
    try {
      await api.deleteSession(s.key);
      toast('已删除', 'ok');
      list.reload();
    } catch (err) { toastErr(err); }
    finally { setBusyKey(''); }
  };

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>会话</h1>
        <div class="sub">已持久化的上游会话；删除后下次请求会重新建立，历史前缀缓存失效。</div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${list.reload}>刷新</button>
      </div>
    </div>

    <div class="toolbar">
      <input value=${q} onInput=${(e) => setQ(e.target.value)}
        placeholder="过滤 key / session / 模型 / 目录" spellcheck="false" style="min-width:280px" />
      <span class="muted small">${rows.length} 个${q ? '（已过滤）' : ''}</span>
    </div>

    <${ErrorBox} error=${list.error} onRetry=${list.reload} />

    ${list.loading ? html`<${Loading} />`
      : rows.length === 0 ? html`<div class="card"><${Empty}>
          ${q ? '没有匹配的会话' : '还没有会话；发起一次对话后会自动记录'}
        <//></div>`
      : html`<div class="tablewrap"><table>
          <thead><tr>
            <th>Key</th><th>Session</th><th>Provider</th><th>模型</th><th>Variant</th>
            <th>Agent</th><th>凭据</th><th>目录</th><th>更新时间</th><th>操作</th>
          </tr></thead>
          <tbody>
            ${rows.map((s) => html`<tr>
              <td class="mono">${s.key}</td>
              <td class="mono small">${s.session_id || html`<span class="muted">–</span>`}</td>
              <td class="small">${s.provider_id || '–'}</td>
              <td class="mono">${s.model_id || '–'}</td>
              <td class="small">${s.variant || html`<span class="muted">–</span>`}</td>
              <td class="small">
                ${s.agent || html`<span class="muted">–</span>`}
                ${s.agent_id ? html`<div class="mono small muted">${s.agent_id}</div>` : null}
              </td>
              <td class="mono small">${s.credential || html`<span class="muted">–</span>`}</td>
              <td class="mono small wrap" style="max-width:260px">${s.directory || html`<span class="muted">–</span>`}</td>
              <td class="small muted">${fmtUnix(s.updated_at)}</td>
              <td>
                <button class="sm danger" disabled=${busyKey === s.key} onClick=${() => del(s)}>
                  ${busyKey === s.key ? '…' : '删除'}
                </button>
              </td>
            </tr>`)}
          </tbody>
        </table></div>`}
  </div>`;
}
