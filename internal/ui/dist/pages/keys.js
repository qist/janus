// pages/keys.js —— API Keys：列表 / 签发 / 轮换 / 吊销。
//
// 明文 key 只在创建与轮换的响应里出现一次，之后库里只有 SHA-256，所以
// 每次签发都用弹窗强制用户复制。

import { html } from '../html.js';
import { Fragment } from 'preact';
import { useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty, Modal, Field, toast, toastErr, confirmDialog, CopyBtn } from '../components.js';
import { fmtUnix } from '../util.js';

const KINDS = [
  { v: 'client', label: 'client（终端用户 / 应用）', hint: '走 /v1/*，可用 model:* 作用域' },
  { v: 'agent', label: 'agent（harness）', hint: '给 OpenCode / Codex 等 harness 使用' },
  { v: 'tool', label: 'tool（工具 / MCP）', hint: 'session:* 作用域，用于 MCP 工具调用' },
  { v: 'admin', label: 'admin（控制面）', hint: '可读写所有控制面 API，慎用' },
];

const SCOPE_HINT = {
  client: 'model:*',
  agent: 'model:*',
  tool: 'session:*',
  admin: 'admin:*',
};

function KeyForm({ onClose, onCreated }) {
  const [f, setF] = useState({
    kind: 'client', name: '', scopes: SCOPE_HINT.client,
    provider_ids: '', credential_ids: '', days: '',
  });
  const [busy, setBusy] = useState(false);
  const set = (k) => (v) => setF((s) => ({ ...s, [k]: v }));

  const changeKind = (kind) => {
    setF((s) => ({ ...s, kind, scopes: SCOPE_HINT[kind] || '' }));
  };

  const save = async (e) => {
    if (e) e.preventDefault();
    const body = {
      kind: f.kind,
      name: f.name.trim() || f.kind,
      scopes: splitList(f.scopes),
      provider_ids: splitList(f.provider_ids),
      credential_ids: splitList(f.credential_ids),
    };
    const days = parseInt(f.days, 10);
    if (days > 0) body.expires_at = Math.floor(Date.now() / 1000) + days * 86400;

    setBusy(true);
    try {
      const res = await api.createKey(body);
      onCreated(res);
    } catch (err) { toastErr(err); }
    finally { setBusy(false); }
  };

  return html`<${Modal} title="签发 API Key" onClose=${onClose}
    footer=${html`<${Fragment}>
      <button onClick=${onClose}>取消</button>
      <button class="primary" onClick=${save} disabled=${busy}>${busy ? '签发中…' : '签发'}</button>
    <//>`}>
    <form onSubmit=${save}>
      <${Field} label="类型">
        <select value=${f.kind} onChange=${(e) => changeKind(e.target.value)}>
          ${KINDS.map((k) => html`<option value=${k.v}>${k.label}</option>`)}
        </select>
      <//>
      <div class="small muted" style="margin:-6px 0 12px">
        ${(KINDS.find((k) => k.v === f.kind) || {}).hint || ''}
      </div>
      <${Field} label="名称" hint="仅用于辨认，可重复">
        <input value=${f.name} onInput=${(e) => set('name')(e.target.value)} placeholder="laptop / ci / opencode" />
      <//>
      <${Field} label="作用域" hint="逗号分隔；留空则按类型取默认值">
        <input value=${f.scopes} onInput=${(e) => set('scopes')(e.target.value)} spellcheck="false" />
      <//>
      <div class="formrow">
        <${Field} label="限制 Provider（可空）" hint="留空 = 全部">
          <input value=${f.provider_ids} onInput=${(e) => set('provider_ids')(e.target.value)} placeholder="openai, anthropic" spellcheck="false" />
        <//>
        <${Field} label="限制凭据（可空）" hint="留空 = 全部">
          <input value=${f.credential_ids} onInput=${(e) => set('credential_ids')(e.target.value)} placeholder="cred_xxx" spellcheck="false" />
        <//>
      </div>
      <${Field} label="有效期（天，可空）" hint="留空 = 永不过期">
        <input type="number" value=${f.days} onInput=${(e) => set('days')(e.target.value)} placeholder="90" />
      <//>
      <div class="alert">明文 key 只在签发后显示一次，请立即保存。</div>
    </form>
  <//>`;
}

function RevealKey({ row, plain, onClose }) {
  return html`<${Modal} title="请立即保存这个 key" onClose=${onClose}
    footer=${html`<${Fragment}>
      <${CopyBtn} text=${plain} label="复制 key" />
      <button class="primary" onClick=${onClose}>我已保存</button>
    <//>`}>
    <div class="alert">关闭后无法再次查看明文；丢失只能轮换。</div>
    <${Field} label=${row.kind + ' · ' + (row.name || row.id)}>
      <input class="mono" value=${plain} readOnly onFocus=${(e) => e.target.select()} />
    <//>
    <div class="kv">
      <span class="k">id</span><span class="mono">${row.id}</span>
      <span class="k">scopes</span><span class="mono">${(row.scopes || []).join(', ') || '–'}</span>
      <span class="k">过期</span><span>${fmtUnix(row.expires_at)}</span>
    </div>
  <//>`;
}

function splitList(s) {
  return String(s || '').split(',').map((x) => x.trim()).filter(Boolean);
}

export default function Keys() {
  const list = useLoad(() => api.keys(), []);
  const [kind, setKind] = useState('');
  const [creating, setCreating] = useState(false);
  const [reveal, setReveal] = useState(null); // {row, plain}
  const [busyId, setBusyId] = useState('');

  const rows = ((list.data && list.data.keys) || []).filter((k) => !kind || k.kind === kind);

  const rotate = async (k) => {
    const ok = await confirmDialog({
      title: '轮换 key',
      message: html`轮换 <b>${k.name || k.id}</b>？旧 key 立即失效，使用它的客户端需要更新配置。`,
      danger: true, okText: '轮换',
    });
    if (!ok) return;
    setBusyId(k.id);
    try {
      const res = await api.rotateKey(k.id);
      setReveal({ row: res, plain: res.key });
      list.reload();
    } catch (err) { toastErr(err); }
    finally { setBusyId(''); }
  };

  const revoke = async (k) => {
    const ok = await confirmDialog({
      title: '吊销 key',
      message: html`吊销 <b>${k.name || k.id}</b>？此操作不可恢复。`,
      danger: true, okText: '吊销',
    });
    if (!ok) return;
    setBusyId(k.id);
    try {
      await api.revokeKey(k.id);
      toast('已吊销', 'ok');
      list.reload();
    } catch (err) { toastErr(err); }
    finally { setBusyId(''); }
  };

  const toggleRevoked = async (k) => {
    // 只提供吊销；恢复需要重新签发（库里的 hash 已不可逆）。
    if (!k.revoked) { await revoke(k); return; }
    toast('已吊销的 key 无法恢复，请重新签发', 'warn');
  };

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>API Keys</h1>
        <div class="sub">客户端、agent 与控制面都用同一套 key；明文只在签发时出现一次。</div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${list.reload}>刷新</button>
        <button class="primary" onClick=${() => setCreating(true)}>签发 key</button>
      </div>
    </div>

    <div class="toolbar">
      <select value=${kind} onChange=${(e) => setKind(e.target.value)}>
        <option value="">全部类型</option>
        ${KINDS.map((k) => html`<option value=${k.v}>${k.v}</option>`)}
      </select>
      <span class="muted small">${rows.length} 个</span>
    </div>

    <${ErrorBox} error=${list.error} onRetry=${list.reload} />

    ${list.loading ? html`<${Loading} />`
      : rows.length === 0 ? html`<div class="card"><${Empty}>还没有 API Key<//></div>`
      : html`<div class="tablewrap"><table>
          <thead><tr>
            <th>名称</th><th>类型</th><th>前缀</th><th>作用域</th>
            <th>限制</th><th>最近使用</th><th>过期</th><th>状态</th><th>操作</th>
          </tr></thead>
          <tbody>
            ${rows.map((k) => html`<tr>
              <td>${k.name || html`<span class="muted">–</span>`}<div class="small muted mono">${k.id}</div></td>
              <td><span class="badge acc">${k.kind}</span></td>
              <td class="mono small">${k.prefix}…</td>
              <td class="mono small wrap" style="max-width:220px">${(k.scopes || []).join(', ') || '–'}</td>
              <td class="small">
                ${(k.provider_ids || []).length ? html`<div>providers: ${k.provider_ids.join(', ')}</div>` : null}
                ${(k.credential_ids || []).length ? html`<div>credentials: ${k.credential_ids.join(', ')}</div>` : null}
                ${!(k.provider_ids || []).length && !(k.credential_ids || []).length ? html`<span class="muted">全部</span>` : null}
              </td>
              <td class="small muted">${fmtUnix(k.last_used_at)}</td>
              <td class="small muted">${k.expires_at ? fmtUnix(k.expires_at) : '永不'}</td>
              <td>${k.revoked
                ? html`<span class="badge err">已吊销</span>`
                : html`<span class="badge ok">有效</span>`}</td>
              <td>
                <div class="btnrow">
                  <button class="sm ghost" disabled=${busyId === k.id || k.revoked} onClick=${() => rotate(k)}>
                    ${busyId === k.id ? '…' : '轮换'}
                  </button>
                  <button class=${'sm ' + (k.revoked ? 'ghost' : 'danger')} disabled=${busyId === k.id} onClick=${() => toggleRevoked(k)}>
                    ${k.revoked ? '已吊销' : '吊销'}
                  </button>
                </div>
              </td>
            </tr>`)}
          </tbody>
        </table></div>`}

    ${creating ? html`<${KeyForm}
      onClose=${() => setCreating(false)}
      onCreated=${(res) => { setCreating(false); list.reload(); setReveal({ row: res, plain: res.key }); }}
    />` : null}

    ${reveal ? html`<${RevealKey} row=${reveal.row} plain=${reveal.plain} onClose=${() => setReveal(null)} />` : null}
  </div>`;
}
