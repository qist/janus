// pages/agents.js —— harness 注册表（opencode / codex / claude-code / direct）。
//
// 每条 agent 记录描述「janus 怎么连它」；provision 反向生成「它怎么连 janus」
// 的配置片段（base_url / 环境变量 / opencode.json 片段）。

import { html } from '../html.js';
import { Fragment } from 'preact';
import { useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty, Modal, Field, toast, toastErr, confirmDialog, CopyBtn, Toggle } from '../components.js';
import { fmtUnix, jsonPretty } from '../util.js';

const KINDS = ['opencode', 'direct', 'cli'];
const AUTH_KINDS = [
  { v: 'basic', label: 'basic（用户名 + 密码）' },
  { v: 'bearer', label: 'bearer（token）' },
  { v: 'none', label: 'none（无鉴权）' },
];

const SECRET_TEMPLATE = {
  basic: '{\n  "username": "opencode",\n  "password": ""\n}',
  bearer: '{\n  "token": ""\n}',
  none: '{}',
};

function AgentForm({ initial, onClose, onSaved }) {
  const isNew = !initial;
  const [f, setF] = useState(initial ? {
    id: initial.id, name: initial.name || '', kind: initial.kind || 'opencode',
    base_url: initial.base_url || '', auth_kind: initial.auth_kind || 'basic',
    api_key_id: initial.api_key_id || '', enabled: !!initial.enabled,
    secretText: '', configText: initial.config ? jsonPretty(initial.config) : '',
  } : {
    id: '', name: '', kind: 'opencode', base_url: '', auth_kind: 'basic',
    api_key_id: '', enabled: true, secretText: '', configText: '',
  });
  const [busy, setBusy] = useState(false);
  const set = (k) => (v) => setF((s) => ({ ...s, [k]: v }));

  const save = async (e) => {
    if (e) e.preventDefault();
    if (!f.id.trim()) { toast('ID 必填', 'err'); return; }

    let secret;
    if (f.secretText.trim()) {
      try { secret = JSON.parse(f.secretText); }
      catch (err) { toast('Secret 不是合法 JSON：' + err.message, 'err'); return; }
    }
    let config;
    if (f.configText.trim()) {
      try { config = JSON.parse(f.configText); }
      catch (err) { toast('Config 不是合法 JSON：' + err.message, 'err'); return; }
    }

    const body = {
      name: f.name.trim() || f.id.trim(),
      kind: f.kind,
      base_url: f.base_url.trim(),
      auth_kind: f.auth_kind,
      enabled: f.enabled,
    };
    if (isNew) body.id = f.id.trim();
    if (f.api_key_id.trim()) body.api_key_id = f.api_key_id.trim();
    if (secret !== undefined) body.secret = secret;
    if (config !== undefined) body.config = config;

    setBusy(true);
    try {
      if (isNew) await api.createAgent(body);
      else await api.patchAgent(initial.id, body);
      toast('已保存', 'ok');
      onSaved();
    } catch (err) { toastErr(err); }
    finally { setBusy(false); }
  };

  return html`<${Modal} title=${isNew ? '注册 Agent' : '编辑 ' + initial.id} onClose=${onClose} wide
    footer=${html`<${Fragment}>
      <button onClick=${onClose}>取消</button>
      <button class="primary" onClick=${save} disabled=${busy}>${busy ? '保存中…' : '保存'}</button>
    <//>`}>
    <form onSubmit=${save}>
      <div class="formrow">
        <${Field} label="ID" hint="创建后不可改">
          <input value=${f.id} onInput=${(e) => set('id')(e.target.value)} disabled=${!isNew} placeholder="opencode" />
        <//>
        <${Field} label="名称">
          <input value=${f.name} onInput=${(e) => set('name')(e.target.value)} placeholder="OpenCode" />
        <//>
      </div>
      <div class="formrow">
        <${Field} label="类型">
          <select value=${f.kind} onChange=${(e) => set('kind')(e.target.value)}>
            ${KINDS.map((k) => html`<option value=${k}>${k}</option>`)}
          </select>
        <//>
        <${Field} label="鉴权方式">
          <select value=${f.auth_kind} onChange=${(e) => set('auth_kind')(e.target.value)}>
            ${AUTH_KINDS.map((k) => html`<option value=${k.v}>${k.label}</option>`)}
          </select>
        <//>
      </div>
      <${Field} label="Base URL" hint="harness 的地址；留空则用 runtime 的 upstream">
        <input value=${f.base_url} onInput=${(e) => set('base_url')(e.target.value)} placeholder="http://127.0.0.1:4096" spellcheck="false" />
      <//>
      <${Field} label="Agent Key ID（可空）" hint="反向注入给它的 agent key；在 API Keys 页签发">
        <input value=${f.api_key_id} onInput=${(e) => set('api_key_id')(e.target.value)} placeholder="key_xxx" spellcheck="false" />
      <//>
      <${Field} label=${isNew ? 'Secret（JSON）' : 'Secret（JSON，留空则不修改）'}
        hint=${'按鉴权方式填写，当前模板：' + f.auth_kind}>
        <textarea value=${f.secretText} onInput=${(e) => set('secretText')(e.target.value)}
          placeholder=${SECRET_TEMPLATE[f.auth_kind] || '{}'} spellcheck="false" style="min-height:100px" />
      <//>
      <${Field} label="Config（JSON，可空）" hint="harness 特定配置，原样保存">
        <textarea value=${f.configText} onInput=${(e) => set('configText')(e.target.value)} placeholder="{}" spellcheck="false" />
      <//>
      <${Toggle} checked=${f.enabled} onChange=${set('enabled')} label="启用" />
    </form>
  <//>`;
}

function ProvisionModal({ id, onClose }) {
  const res = useLoad(() => api.provisionAgent(id), [id]);
  const snippet = res.data;
  const text = snippet ? jsonPretty(snippet) : '';

  return html`<${Modal} title="接入配置" onClose=${onClose} wide
    footer=${html`<${Fragment}>
      ${snippet ? html`<${CopyBtn} text=${text} label="复制 JSON" />` : null}
      <button class="primary" onClick=${onClose}>关闭</button>
    <//>`}>
    <${ErrorBox} error=${res.error} onRetry=${res.reload} />
    ${res.loading ? html`<${Loading} />` : null}
    ${snippet ? html`<div>
      <div class="alert">
        把片段里的 <code>&lt;agent key&gt;</code> 换成真实 key；
        key 在「API Keys」页签发，只显示一次。
        ${snippet.tool_mode ? html`当前工具执行模式：<code>${snippet.tool_mode}</code>。` : null}
      </div>
      ${snippet.env ? html`<div class="cardhead"><h2>环境变量</h2></div>
        <div class="kv">
          ${Object.entries(snippet.env).map(([k, v]) => html`<${Fragment}>
            <span class="k mono">${k}</span><span class="mono wrap">${v}</span>
          <//>`)}
        </div>` : null}
      ${(snippet.files || []).map((f) => html`<${Fragment}>
        <div class="cardhead" style="margin-top:16px">
          <h2>配置文件：<code>${f.path}</code></h2>
        </div>
        <pre class="code">${f.content}</pre>
      <//>`)}
      ${snippet.command ? html`<div class="cardhead" style="margin-top:16px"><h2>启动命令</h2></div>
        <pre class="code">${snippet.command.join(' ')}</pre>` : null}
      ${snippet.notes ? html`<ul class="notes">
        ${snippet.notes.map((n) => html`<li>${n}</li>`)}
      </ul>` : null}
      <div class="cardhead" style="margin-top:16px"><h2>原始响应</h2></div>
      <pre class="code">${text}</pre>
    </div>` : null}
  <//>`;
}

export default function Agents() {
  const list = useLoad(() => api.agents(), []);
  const [editing, setEditing] = useState(null); // {isNew, row}
  const [provision, setProvision] = useState('');
  const [busyId, setBusyId] = useState('');

  const rows = (list.data && list.data.agents) || [];

  const toggle = async (a) => {
    setBusyId(a.id);
    try {
      await api.patchAgent(a.id, { enabled: !a.enabled });
      list.reload();
    } catch (err) { toastErr(err); }
    finally { setBusyId(''); }
  };

  const del = async (a) => {
    const ok = await confirmDialog({
      title: '删除 Agent',
      message: html`确定删除 <b>${a.id}</b>？关联的 key 不会自动吊销。`,
      danger: true, okText: '删除',
    });
    if (!ok) return;
    try {
      await api.deleteAgent(a.id);
      toast('已删除', 'ok');
      list.reload();
    } catch (err) { toastErr(err); }
  };

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>Agents</h1>
        <div class="sub">harness 注册表：janus 通过它把请求转给 OpenCode / Codex 等。</div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${list.reload}>刷新</button>
        <button class="primary" onClick=${() => setEditing({ isNew: true, row: null })}>注册 Agent</button>
      </div>
    </div>

    <${ErrorBox} error=${list.error} onRetry=${list.reload} />

    ${list.loading ? html`<${Loading} />`
      : rows.length === 0 ? html`<div class="card"><${Empty}>
          还没有 Agent。点击右上角「注册 Agent」添加一条 <code>opencode</code>。
        <//></div>`
      : html`<div class="tablewrap"><table>
          <thead><tr>
            <th>ID</th><th>名称</th><th>类型</th><th>Base URL</th><th>鉴权</th>
            <th>Key</th><th>状态</th><th>最近心跳</th><th>操作</th>
          </tr></thead>
          <tbody>
            ${rows.map((a) => html`<tr>
              <td class="mono">${a.id}</td>
              <td>${a.name || '–'}</td>
              <td><span class="badge acc">${a.kind}</span></td>
              <td class="mono small wrap" style="max-width:240px">${a.base_url || html`<span class="muted">（runtime upstream）</span>`}</td>
              <td class="small">${a.auth_kind}${a.has_secret ? html` <span class="badge ok">已配</span>` : null}</td>
              <td class="mono small">${a.api_key_id || html`<span class="muted">–</span>`}</td>
              <td>
                ${a.enabled ? html`<span class="badge ok">启用</span>` : html`<span class="badge">停用</span>`}
                ${a.status ? html` <span class=${'badge ' + (a.status === 'ready' ? 'ok' : a.status === 'down' ? 'err' : '')}>${a.status}</span>` : null}
              </td>
              <td class="small muted">${fmtUnix(a.last_seen)}</td>
              <td>
                <div class="btnrow">
                  <button class="sm ghost" onClick=${() => setProvision(a.id)}>接入配置</button>
                  <button class="sm ghost" disabled=${busyId === a.id} onClick=${() => toggle(a)}>${a.enabled ? '停用' : '启用'}</button>
                  <button class="sm ghost" onClick=${() => setEditing({ isNew: false, row: a })}>编辑</button>
                  <button class="sm danger" onClick=${() => del(a)}>删除</button>
                </div>
              </td>
            </tr>`)}
          </tbody>
        </table></div>`}

    ${editing ? html`<${AgentForm}
      initial=${editing.isNew ? null : editing.row}
      onClose=${() => setEditing(null)}
      onSaved=${() => { setEditing(null); list.reload(); }}
    />` : null}

    ${provision ? html`<${ProvisionModal} id=${provision} onClose=${() => setProvision('')} />` : null}
  </div>`;
}
