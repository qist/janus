// pages/providers.js —— 厂商端点：列表 / 新建 / 编辑 / 测试 / 发现模型。

import { html } from '../html.js';
import { Fragment } from 'preact';
import { useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty, Modal, Field, toast, toastErr, confirmDialog, Toggle } from '../components.js';
import { fmtUnix } from '../util.js';

const PROTOCOLS = [
  { v: 'openai-chat', label: 'OpenAI Chat Completions' },
  { v: 'openai-responses', label: 'OpenAI Responses' },
  { v: 'anthropic-messages', label: 'Anthropic Messages' },
];

const AUTH_KINDS = [
  { v: 'api_key', label: 'api_key（Bearer / x-api-key）' },
  { v: 'oauth', label: 'oauth（access_token）' },
  { v: 'none', label: 'none（无鉴权）' },
];

const BASE_HINT = {
  'openai-chat': 'https://api.openai.com/v1',
  'openai-responses': 'https://api.openai.com/v1',
  'anthropic-messages': 'https://api.anthropic.com',
};

function emptyForm() {
  return { id: '', name: '', type: 'openai-chat', base_url: '', auth_kind: 'api_key', headersText: '', enabled: true };
}

function toForm(p) {
  return {
    id: p.id, name: p.name || '', type: p.type || 'openai-chat',
    base_url: p.base_url || '', auth_kind: p.auth_kind || 'api_key',
    headersText: p.headers ? JSON.stringify(p.headers, null, 2) : '',
    enabled: !!p.enabled,
  };
}

function ProviderForm({ initial, isNew, onClose, onSaved }) {
  const [f, setF] = useState(initial);
  const [busy, setBusy] = useState(false);
  const set = (k) => (v) => setF((s) => ({ ...s, [k]: v }));

  const save = async (e) => {
    if (e) e.preventDefault();
    if (!f.id.trim()) { toast('ID 必填', 'err'); return; }
    let headers;
    if (f.headersText.trim()) {
      try { headers = JSON.parse(f.headersText); }
      catch (err) { toast('附加头不是合法 JSON：' + err.message, 'err'); return; }
    }
    const body = {
      name: f.name.trim() || f.id.trim(),
      type: f.type,
      base_url: f.base_url.trim(),
      auth_kind: f.auth_kind,
      enabled: f.enabled,
    };
    if (isNew) body.id = f.id.trim();
    if (headers !== undefined) body.headers = headers;

    setBusy(true);
    try {
      if (isNew) await api.createProvider(body);
      else await api.patchProvider(initial.id, body);
      toast('已保存', 'ok');
      onSaved();
    } catch (err) {
      toastErr(err);
    } finally {
      setBusy(false);
    }
  };

  return html`<${Modal} title=${isNew ? '新建 Provider' : '编辑 ' + initial.id} onClose=${onClose}
    footer=${html`<${Fragment}>
      <button onClick=${onClose}>取消</button>
      <button class="primary" onClick=${save} disabled=${busy}>${busy ? '保存中…' : '保存'}</button>
    <//>`}>
    <form onSubmit=${save}>
      <div class="formrow">
        <${Field} label="ID" hint="唯一标识，创建后不可改，如 openai / anthropic">
          <input value=${f.id} onInput=${(e) => set('id')(e.target.value)} disabled=${!isNew} placeholder="openai" />
        <//>
        <${Field} label="名称">
          <input value=${f.name} onInput=${(e) => set('name')(e.target.value)} placeholder="OpenAI" />
        <//>
      </div>
      <${Field} label="协议">
        <select value=${f.type} onChange=${(e) => set('type')(e.target.value)}>
          ${PROTOCOLS.map((p) => html`<option value=${p.v}>${p.label}</option>`)}
        </select>
      <//>
      <${Field} label="Base URL" hint=${'示例：' + (BASE_HINT[f.type] || '')}>
        <input value=${f.base_url} onInput=${(e) => set('base_url')(e.target.value)} placeholder=${BASE_HINT[f.type] || ''} spellcheck="false" />
      <//>
      <${Field} label="鉴权方式">
        <select value=${f.auth_kind} onChange=${(e) => set('auth_kind')(e.target.value)}>
          ${AUTH_KINDS.map((p) => html`<option value=${p.v}>${p.label}</option>`)}
        </select>
      <//>
      <${Field} label="附加请求头（JSON，可空）" hint='如 {"X-Title":"janus"}'>
        <textarea value=${f.headersText} onInput=${(e) => set('headersText')(e.target.value)} placeholder="{}" spellcheck="false" />
      <//>
      <${Toggle} checked=${f.enabled} onChange=${set('enabled')} label="启用" />
    </form>
  <//>`;
}

export default function Providers() {
  const list = useLoad(() => api.providers(), []);
  const [editing, setEditing] = useState(null); // null | {isNew, initial}
  const [busyId, setBusyId] = useState('');

  const rows = (list.data && list.data.providers) || [];

  const doTest = async (p) => {
    setBusyId(p.id);
    try {
      await api.testProvider(p.id);
      toast(`${p.id} 连接正常`, 'ok');
    } catch (err) {
      toast(`${p.id} 测试失败：${err.message}`, 'err');
    } finally {
      setBusyId('');
    }
  };

  const doDiscover = async (p) => {
    setBusyId(p.id);
    try {
      const res = await api.discoverProvider(p.id);
      toast(`已同步 ${res.count} 个模型`, 'ok');
    } catch (err) {
      toast(`同步失败：${err.message}`, 'err');
    } finally {
      setBusyId('');
    }
  };

  const doDelete = async (p) => {
    const ok = await confirmDialog({
      title: '删除 Provider',
      message: html`确定删除 <b>${p.id}</b>？其凭据与模型目录不会自动删除。`,
      danger: true, okText: '删除',
    });
    if (!ok) return;
    try {
      await api.deleteProvider(p.id);
      toast('已删除', 'ok');
      list.reload();
    } catch (err) { toastErr(err); }
  };

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>Providers</h1>
        <div class="sub">厂商端点与协议类型；凭据在「凭据」页单独管理。</div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${list.reload}>刷新</button>
        <button class="primary" onClick=${() => setEditing({ isNew: true, initial: emptyForm() })}>新建 Provider</button>
      </div>
    </div>

    <${ErrorBox} error=${list.error} onRetry=${list.reload} />

    ${list.loading ? html`<${Loading} />`
      : rows.length === 0 ? html`<div class="card"><${Empty}>
          还没有 Provider。先新建一个端点（如 OpenAI / Anthropic），再添加凭据。
        <//></div>`
      : html`<div class="tablewrap"><table>
          <thead><tr>
            <th>ID</th><th>名称</th><th>协议</th><th>Base URL</th><th>鉴权</th>
            <th>状态</th><th>更新</th><th>操作</th>
          </tr></thead>
          <tbody>
            ${rows.map((p) => html`<tr>
              <td class="mono">${p.id}</td>
              <td>${p.name || '–'}</td>
              <td><span class="badge acc">${p.type}</span></td>
              <td class="mono small wrap">${p.base_url || '–'}</td>
              <td class="small">${p.auth_kind}</td>
              <td>${p.enabled
                ? html`<span class="badge ok"><span class="dot ok"></span>启用</span>`
                : html`<span class="badge">停用</span>`}</td>
              <td class="small muted">${fmtUnix(p.updated_at)}</td>
              <td>
                <div class="btnrow">
                  <button class="sm ghost" onClick=${() => setEditing({ isNew: false, initial: toForm(p) })}>编辑</button>
                  <button class="sm ghost" disabled=${busyId === p.id} onClick=${() => doTest(p)}>
                    ${busyId === p.id ? '…' : '测试'}
                  </button>
                  <button class="sm ghost" disabled=${busyId === p.id} onClick=${() => doDiscover(p)}>同步模型</button>
                  <button class="sm danger" onClick=${() => doDelete(p)}>删除</button>
                </div>
              </td>
            </tr>`)}
          </tbody>
        </table></div>`}

    ${editing ? html`<${ProviderForm}
      isNew=${editing.isNew}
      initial=${editing.initial}
      onClose=${() => setEditing(null)}
      onSaved=${() => { setEditing(null); list.reload(); }}
    />` : null}
  </div>`;
}
