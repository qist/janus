// pages/routes.js —— 路由规则 + 内置解析器（实时显示命中链）。

import { html } from '../html.js';
import { Fragment } from 'preact';
import { useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty, Modal, Field, toast, toastErr, confirmDialog, Toggle } from '../components.js';

const SCOPES = [
  { v: 'global', label: 'global（所有请求）' },
  { v: 'provider', label: 'provider（按 provider）' },
  { v: 'credential', label: 'credential（按凭据）' },
  { v: 'role', label: 'role（janus/<role>）' },
];

function RouteForm({ initial, onClose, onSaved }) {
  const isNew = !initial;
  const [f, setF] = useState(initial ? {
    scope: initial.scope || 'global', scope_id: initial.scope_id || '',
    alias: initial.alias || '', target: initial.target || '',
    priority: String(initial.priority || 0), enabled: !!initial.enabled,
  } : { scope: 'global', scope_id: '', alias: '', target: '', priority: '0', enabled: true });
  const [busy, setBusy] = useState(false);
  const set = (k) => (v) => setF((s) => ({ ...s, [k]: v }));

  const save = async (e) => {
    if (e) e.preventDefault();
    if (!f.alias.trim() || !f.target.trim()) { toast('alias 与 target 必填', 'err'); return; }
    const body = {
      scope: f.scope,
      scope_id: f.scope === 'global' ? '' : f.scope_id.trim(),
      alias: f.alias.trim(),
      target: f.target.trim(),
      priority: parseInt(f.priority, 10) || 0,
      enabled: f.enabled,
    };
    setBusy(true);
    try {
      if (isNew) await api.createRoute(body);
      else await api.patchRoute(initial.id, body);
      toast('已保存', 'ok');
      onSaved();
    } catch (err) { toastErr(err); }
    finally { setBusy(false); }
  };

  return html`<${Modal} title=${isNew ? '新建路由规则' : '编辑规则'} onClose=${onClose}
    footer=${html`<${Fragment}>
      <button onClick=${onClose}>取消</button>
      <button class="primary" onClick=${save} disabled=${busy}>${busy ? '保存中…' : '保存'}</button>
    <//>`}>
    <form onSubmit=${save}>
      <div class="formrow">
        <${Field} label="作用域">
          <select value=${f.scope} onChange=${(e) => set('scope')(e.target.value)}>
            ${SCOPES.map((s) => html`<option value=${s.v}>${s.label}</option>`)}
          </select>
        <//>
        <${Field} label="作用域 ID" hint="provider / credential 的 ID；role 填角色名">
          <input value=${f.scope_id} onInput=${(e) => set('scope_id')(e.target.value)}
            disabled=${f.scope === 'global'} placeholder=${f.scope === 'global' ? '（全局无需填写）' : ''} />
        <//>
      </div>
      <div class="formrow">
        <${Field} label="别名（alias）" hint="小写；尾部 * 为前缀通配">
          <input value=${f.alias} onInput=${(e) => set('alias')(e.target.value)} placeholder="gpt-4o*" spellcheck="false" />
        <//>
        <${Field} label="目标（target）" hint="provider/model[:variant]">
          <input value=${f.target} onInput=${(e) => set('target')(e.target.value)} placeholder="openai/gpt-4o" spellcheck="false" />
        <//>
      </div>
      <div class="formrow">
        <${Field} label="优先级" hint="数字越小越优先">
          <input type="number" value=${f.priority} onInput=${(e) => set('priority')(e.target.value)} />
        <//>
        <${Field} label="状态">
          <div style="padding-top:6px"><${Toggle} checked=${f.enabled} onChange=${set('enabled')} label="启用" /></div>
        <//>
      </div>
    </form>
  <//>`;
}

// ---------- 解析器 ----------
function Resolver() {
  const [model, setModel] = useState('');
  const [protocol, setProtocol] = useState('');
  const [credential, setCredential] = useState('');
  const [out, setOut] = useState(null);
  const [err, setErr] = useState(null);
  const [busy, setBusy] = useState(false);

  const run = async (e) => {
    if (e) e.preventDefault();
    if (!model.trim()) { setErr(new Error('请输入模型名')); return; }
    setBusy(true); setErr(null); setOut(null);
    try {
      const res = await api.resolveRoute({
        model: model.trim(),
        protocol,
        credential: credential.trim(),
      });
      setOut(res);
    } catch (e2) { setErr(e2); }
    finally { setBusy(false); }
  };

  return html`<div class="card">
    <div class="cardhead"><h2>路由解析器</h2>
      <span class="badge">实时</span>
    </div>
    <form onSubmit=${run}>
      <${Field} label="模型名">
        <input value=${model} onInput=${(e) => setModel(e.target.value)} placeholder="gpt-4o / janus/coder" spellcheck="false" />
      <//>
      <div class="formrow">
        <${Field} label="协议（可空）">
          <select value=${protocol} onChange=${(e) => setProtocol(e.target.value)}>
            <option value="">自动</option>
            <option value="openai-chat">openai-chat</option>
            <option value="openai-responses">openai-responses</option>
            <option value="anthropic-messages">anthropic-messages</option>
          </select>
        <//>
        <${Field} label="凭据提示（可空）">
          <input value=${credential} onInput=${(e) => setCredential(e.target.value)} placeholder="label 或 id" />
        <//>
      </div>
      <button class="primary" disabled=${busy}>${busy ? '解析中…' : '解析'}</button>
    </form>

    ${err ? html`<div class="alert err" style="margin-top:12px">
      <div>${err.message}</div>
      ${err.details && err.details.candidates
        ? html`<div class="small" style="margin-top:6px">候选：${err.details.candidates.join(', ')}</div>` : null}
    </div>` : null}

    ${out ? html`<div style="margin-top:12px">
      <div class="kv">
        <span class="k">provider</span><span class="mono">${out.provider_id}</span>
        <span class="k">credential</span><span class="mono">${out.credential_id || '–'}</span>
        <span class="k">upstream</span><span class="mono">${out.upstream_id}</span>
        ${out.variant ? html`<span class="k">variant</span><span class="mono">${out.variant}</span>` : null}
        <span class="k">passthrough</span><span>${out.passthrough ? '是' : '否'}</span>
      </div>
      <pre class="code" style="margin-top:10px">${out.rule || '（无规则说明）'}</pre>
    </div>` : null}
  </div>`;
}

export default function Routes() {
  const list = useLoad(() => api.routes(), []);
  const [editing, setEditing] = useState(null);

  const rows = (list.data && list.data.routes) || [];

  const toggle = async (r) => {
    try { await api.patchRoute(r.id, { enabled: !r.enabled }); list.reload(); }
    catch (err) { toastErr(err); }
  };

  const del = async (r) => {
    const ok = await confirmDialog({
      title: '删除规则',
      message: html`确定删除规则 <b>${r.alias}</b> → <code>${r.target}</code>？`,
      danger: true, okText: '删除',
    });
    if (!ok) return;
    try { await api.deleteRoute(r.id); toast('已删除', 'ok'); list.reload(); }
    catch (err) { toastErr(err); }
  };

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>路由规则</h1>
        <div class="sub">按作用域把别名映射到 provider/model；数字小的优先级高。</div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${list.reload}>刷新</button>
        <button class="primary" onClick=${() => setEditing({ isNew: true })}>新建规则</button>
      </div>
    </div>

    <${ErrorBox} error=${list.error} onRetry=${list.reload} />

    <div class="split">
      <div>
        ${list.loading ? html`<${Loading} />`
          : rows.length === 0 ? html`<div class="card"><${Empty}>还没有路由规则<//></div>`
          : html`<div class="tablewrap"><table>
              <thead><tr>
                <th>作用域</th><th>别名</th><th>目标</th><th class="num">优先级</th><th>状态</th><th>操作</th>
              </tr></thead>
              <tbody>
                ${rows.map((r) => html`<tr>
                  <td><span class="badge">${r.scope}</span>${r.scope_id ? html`<span class="mono small"> ${r.scope_id}</span>` : null}</td>
                  <td class="mono">${r.alias}</td>
                  <td class="mono">${r.target}</td>
                  <td class="num">${r.priority}</td>
                  <td>${r.enabled ? html`<span class="badge ok">启用</span>` : html`<span class="badge">停用</span>`}</td>
                  <td>
                    <div class="btnrow">
                      <button class="sm ghost" onClick=${() => setEditing({ isNew: false, row: r })}>编辑</button>
                      <button class="sm ghost" onClick=${() => toggle(r)}>${r.enabled ? '停用' : '启用'}</button>
                      <button class="sm danger" onClick=${() => del(r)}>删除</button>
                    </div>
                  </td>
                </tr>`)}
              </tbody>
            </table></div>`}
      </div>
      <${Resolver} />
    </div>

    ${editing ? html`<${RouteForm}
      initial=${editing.isNew ? null : editing.row}
      onClose=${() => setEditing(null)}
      onSaved=${() => { setEditing(null); list.reload(); }}
    />` : null}
  </div>`;
}
