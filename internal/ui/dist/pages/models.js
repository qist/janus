// pages/models.js —— 模型目录：按 provider 分组、搜索、启停、能力标签、手工新增。

import { html } from '../html.js';
import { Fragment } from 'preact';
import { useMemo, useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty, Modal, Field, toast, toastErr, confirmDialog, Toggle, CopyBtn } from '../components.js';

function capsLabel(m) {
  const c = m.capabilities || {};
  const out = [];
  if (c.tools) out.push('tools');
  if (Array.isArray(c.input) && c.input.length) out.push('in:' + c.input.join('+'));
  if (Array.isArray(c.output) && c.output.length) out.push('out:' + c.output.join('+'));
  if (c.context) out.push('ctx ' + c.context);
  return out;
}

function ModelForm({ providers, onClose, onSaved }) {
  const [f, setF] = useState({
    provider_id: providers.length ? providers[0].id : '',
    upstream_id: '', display_name: '', enabled: true, capsText: '',
  });
  const [busy, setBusy] = useState(false);
  const set = (k) => (v) => setF((s) => ({ ...s, [k]: v }));

  const save = async (e) => {
    if (e) e.preventDefault();
    if (!f.provider_id || !f.upstream_id.trim()) { toast('provider_id 与 upstream_id 必填', 'err'); return; }
    let capabilities;
    if (f.capsText.trim()) {
      try { capabilities = JSON.parse(f.capsText); }
      catch (err) { toast('能力不是合法 JSON：' + err.message, 'err'); return; }
    }
    const body = {
      provider_id: f.provider_id,
      upstream_id: f.upstream_id.trim(),
      display_name: f.display_name.trim() || f.upstream_id.trim(),
      enabled: f.enabled,
    };
    if (capabilities !== undefined) body.capabilities = capabilities;
    setBusy(true);
    try {
      await api.createModel(body);
      toast('已添加', 'ok');
      onSaved();
    } catch (err) { toastErr(err); }
    finally { setBusy(false); }
  };

  return html`<${Modal} title="手工添加模型" onClose=${onClose}
    footer=${html`<${Fragment}>
      <button onClick=${onClose}>取消</button>
      <button class="primary" onClick=${save} disabled=${busy}>${busy ? '保存中…' : '添加'}</button>
    <//>`}>
    <form onSubmit=${save}>
      <div class="formrow">
        <${Field} label="Provider">
          <select value=${f.provider_id} onChange=${(e) => set('provider_id')(e.target.value)}>
            ${providers.map((p) => html`<option value=${p.id}>${p.id}</option>`)}
          </select>
        <//>
        <${Field} label="上游模型 ID">
          <input value=${f.upstream_id} onInput=${(e) => set('upstream_id')(e.target.value)} placeholder="gpt-4o" spellcheck="false" />
        <//>
      </div>
      <${Field} label="显示名">
        <input value=${f.display_name} onInput=${(e) => set('display_name')(e.target.value)} placeholder="GPT-4o" />
      <//>
      <${Field} label="能力（JSON，可空）" hint='如 {"tools":true,"context":128000,"input":["text","image"]}'>
        <textarea value=${f.capsText} onInput=${(e) => set('capsText')(e.target.value)} placeholder="{}" spellcheck="false" />
      <//>
      <${Toggle} checked=${f.enabled} onChange=${set('enabled')} label="启用" />
    </form>
  <//>`;
}

export default function Models() {
  const provs = useLoad(() => api.providers(), []);
  const models = useLoad(() => api.models(), []);
  const [q, setQ] = useState('');
  const [pid, setPid] = useState('');
  const [onlyEnabled, setOnlyEnabled] = useState(false);
  const [adding, setAdding] = useState(false);

  const providers = (provs.data && provs.data.providers) || [];
  const all = (models.data && models.data.models) || [];

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return all.filter((m) => {
      if (pid && m.provider_id !== pid) return false;
      if (onlyEnabled && !m.enabled) return false;
      if (!needle) return true;
      return (m.id + ' ' + (m.display_name || '') + ' ' + m.upstream_id).toLowerCase().includes(needle);
    });
  }, [all, q, pid, onlyEnabled]);

  const toggle = async (m) => {
    try {
      await api.patchModel(m.id, { enabled: !m.enabled });
      models.reload();
    } catch (err) { toastErr(err); }
  };

  const del = async (m) => {
    const ok = await confirmDialog({
      title: '删除模型',
      message: html`确定从目录删除 <b>${m.id}</b>？`,
      danger: true, okText: '删除',
    });
    if (!ok) return;
    try {
      await api.deleteModel(m.id);
      toast('已删除', 'ok');
      models.reload();
    } catch (err) { toastErr(err); }
  };

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>模型目录</h1>
        <div class="sub">目录决定客户端可选的模型；同步来自 provider 的 /models 端点。</div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${models.reload}>刷新</button>
        <button class="primary" disabled=${providers.length === 0} onClick=${() => setAdding(true)}>手工添加</button>
      </div>
    </div>

    <div class="toolbar">
      <input value=${q} onInput=${(e) => setQ(e.target.value)} placeholder="搜索模型 / 显示名" style="min-width:240px" />
      <select value=${pid} onChange=${(e) => setPid(e.target.value)}>
        <option value="">全部 Provider</option>
        ${providers.map((p) => html`<option value=${p.id}>${p.id}</option>`)}
      </select>
      <label class="switch">
        <input type="checkbox" checked=${onlyEnabled} onChange=${(e) => setOnlyEnabled(e.target.checked)} />
        仅启用
      </label>
      <span class="muted small">${rows.length} / ${all.length}</span>
    </div>

    <${ErrorBox} error=${provs.error || models.error} onRetry=${models.reload} />

    ${models.loading ? html`<${Loading} />`
      : rows.length === 0 ? html`<div class="card"><${Empty}>
          还没有模型。到 <a href="#/providers">Providers</a> 点「同步模型」自动发现。
        <//></div>`
      : html`<div class="tablewrap"><table>
          <thead><tr>
            <th>模型 ID</th><th>显示名</th><th>能力</th><th>来源</th><th>状态</th><th>操作</th>
          </tr></thead>
          <tbody>
            ${rows.map((m) => html`<tr>
              <td class="mono wrap">${m.id}</td>
              <td>${m.display_name || html`<span class="muted">–</span>`}</td>
              <td>${capsLabel(m).map((c) => html`<span class="badge" style="margin-right:4px">${c}</span>`)}</td>
              <td><span class="badge">${m.source}</span></td>
              <td>${m.enabled ? html`<span class="badge ok">启用</span>` : html`<span class="badge">停用</span>`}</td>
              <td>
                <div class="btnrow">
                  <${CopyBtn} text=${m.id} label="复制模型名" small=${true} />
                  <button class="sm ghost" onClick=${() => toggle(m)}>${m.enabled ? '停用' : '启用'}</button>
                  <button class="sm danger" onClick=${() => del(m)}>删除</button>
                </div>
              </td>
            </tr>`)}
          </tbody>
        </table></div>`}

    ${adding ? html`<${ModelForm}
      providers=${providers}
      onClose=${() => setAdding(false)}
      onSaved=${() => { setAdding(false); models.reload(); }}
    />` : null}
  </div>`;
}
