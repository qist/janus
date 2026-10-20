// pages/credentials.js —— 按 provider 分组管理凭据（密钥加密存库，列表只显示键名）。

import { html } from '../html.js';
import { Fragment } from 'preact';
import { useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Empty, Modal, Field, toast, toastErr, confirmDialog, Toggle } from '../components.js';
import { fmtUnix } from '../util.js';

// 每类鉴权方式期望的 secret 键名。
const SECRET_TEMPLATE = {
  api_key: '{\n  "api_key": ""\n}',
  oauth: '{\n  "access_token": "",\n  "refresh_token": "",\n  "token_url": "",\n  "client_id": ""\n}',
  workbuddy: '{\n  "accessToken": "",\n  "refreshToken": "",\n  "domain": "workbuddy.ai",\n  "uid": "",\n  "enterpriseId": ""\n}',
  none: '{}',
};

function templateFor(provider) {
  return SECRET_TEMPLATE[(provider && provider.auth_kind) || 'api_key'] || SECRET_TEMPLATE.api_key;
}

// 能自动续期的鉴权方式（对应 POST /credentials/{id}/refresh）。
function canRefresh(authKind) {
  return authKind === 'oauth' || authKind === 'workbuddy';
}

function CredentialForm({ providers, initial, onClose, onSaved }) {
  const isNew = !initial;
  const [f, setF] = useState(initial ? {
    provider_id: initial.provider_id, label: initial.label || '',
    secretText: '', priority: String(initial.priority || 0), enabled: !!initial.enabled,
  } : {
    provider_id: providers.length ? providers[0].id : '',
    label: '', secretText: templateFor(providers[0]), priority: '0', enabled: true,
  });
  const [busy, setBusy] = useState(false);
  const set = (k) => (v) => setF((s) => ({ ...s, [k]: v }));

  const provider = providers.find((p) => p.id === f.provider_id);
  const template = templateFor(provider);
  // 换 provider 时（仅新建）同步换成对应的 secret 模板，避免填错键名。
  const pickProvider = (pid) => setF((s) => ({
    ...s, provider_id: pid,
    secretText: isNew ? templateFor(providers.find((p) => p.id === pid)) : s.secretText,
  }));

  const save = async (e) => {
    if (e) e.preventDefault();
    let secret;
    if (f.secretText.trim()) {
      try { secret = JSON.parse(f.secretText); }
      catch (err) { toast('Secret 不是合法 JSON：' + err.message, 'err'); return; }
    } else if (isNew) {
      toast('请填写 Secret', 'err'); return;
    }
    const body = {
      label: f.label.trim(),
      priority: parseInt(f.priority, 10) || 0,
      enabled: f.enabled,
    };
    if (isNew) body.provider_id = f.provider_id;
    if (secret !== undefined) body.secret = secret;

    setBusy(true);
    try {
      if (isNew) await api.createCredential(body);
      else await api.patchCredential(initial.id, body);
      toast('已保存', 'ok');
      onSaved();
    } catch (err) {
      toastErr(err);
    } finally {
      setBusy(false);
    }
  };

  return html`<${Modal} title=${isNew ? '新建凭据' : '编辑凭据'} onClose=${onClose}
    footer=${html`<${Fragment}>
      <button onClick=${onClose}>取消</button>
      <button class="primary" onClick=${save} disabled=${busy}>${busy ? '保存中…' : '保存'}</button>
    <//>`}>
    <form onSubmit=${save}>
      <div class="formrow">
        <${Field} label="Provider">
          <select value=${f.provider_id} onChange=${(e) => pickProvider(e.target.value)} disabled=${!isNew}>
            ${providers.map((p) => html`<option value=${p.id}>${p.id}</option>`)}
          </select>
        <//>
        <${Field} label="标签" hint="同一 provider 内唯一，客户端可用 label/model 指定">
          <input value=${f.label} onInput=${(e) => set('label')(e.target.value)} placeholder="personal" />
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
      <${Field}
        label=${isNew ? 'Secret（JSON）' : 'Secret（JSON，留空则不修改）'}
        hint=${'按 provider 鉴权方式填写；期望键名：' + ((provider && provider.auth_kind) || 'api_key')}>
        <textarea value=${f.secretText} onInput=${(e) => set('secretText')(e.target.value)}
          placeholder=${template} spellcheck="false" style="min-height:120px" />
      <//>
      <div class="alert">
        密钥加密后入库，接口只返回键名（如 <code>api_key</code>），不会回显明文。
      </div>
    </form>
  <//>`;
}

export default function Credentials() {
  const provs = useLoad(() => api.providers(), []);
  const creds = useLoad(() => api.credentials(), []);
  const [editing, setEditing] = useState(null); // {mode:'new'|'edit', row}
  const [busyId, setBusyId] = useState('');

  const providers = (provs.data && provs.data.providers) || [];
  const rows = (creds.data && creds.data.credentials) || [];

  const doVerify = async (c) => {
    setBusyId(c.id);
    try {
      await api.verifyCredential(c.id);
      toast('凭据可用', 'ok');
      creds.reload();
    } catch (err) {
      toast('验证失败：' + err.message, 'err');
      creds.reload();
    } finally {
      setBusyId('');
    }
  };

  const doRefresh = async (c) => {
    setBusyId(c.id);
    try {
      await api.refreshCredential(c.id);
      toast('授权已续期', 'ok');
      creds.reload();
    } catch (err) {
      toast('续期失败：' + err.message, 'err');
      creds.reload();
    } finally {
      setBusyId('');
    }
  };

  const doToggle = async (c) => {
    try {
      await api.patchCredential(c.id, { enabled: !c.enabled });
      creds.reload();
    } catch (err) { toastErr(err); }
  };

  const doDelete = async (c) => {
    const ok = await confirmDialog({
      title: '删除凭据',
      message: html`确定删除 <b>${c.provider_id}/${c.label || c.id}</b>？`,
      danger: true, okText: '删除',
    });
    if (!ok) return;
    try {
      await api.deleteCredential(c.id);
      toast('已删除', 'ok');
      creds.reload();
    } catch (err) { toastErr(err); }
  };

  // 按 provider 分组
  const groups = new Map();
  for (const c of rows) {
    if (!groups.has(c.provider_id)) groups.set(c.provider_id, []);
    groups.get(c.provider_id).push(c);
  }

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>凭据</h1>
        <div class="sub">每份凭据独立客户端，可并发使用；失败会进入冷却。</div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${() => { provs.reload(); creds.reload(); }}>刷新</button>
        <button class="primary" disabled=${providers.length === 0}
          onClick=${() => setEditing({ mode: 'new' })}>新建凭据</button>
      </div>
    </div>

    <${ErrorBox} error=${provs.error || creds.error} onRetry=${() => { provs.reload(); creds.reload(); }} />

    ${providers.length === 0 && !provs.loading ? html`<div class="alert">
      还没有 Provider，先到 <a href="#/providers">Providers</a> 建一个端点。
    </div>` : null}

    ${creds.loading ? html`<${Loading} />`
      : rows.length === 0 ? html`<div class="card"><${Empty}>还没有凭据<//></div>`
      : [...groups.entries()].map(([pid, list]) => html`<div class="card" key=${pid}>
          <div class="cardhead">
            <h2>${pid}</h2>
            <span class="badge">${list.length} 份</span>
          </div>
          <div class="tablewrap"><table>
            <thead><tr>
              <th>标签</th><th>鉴权</th><th>Secret 键</th><th class="num">优先级</th><th>状态</th>
              <th>最近使用</th><th>错误</th><th>操作</th>
            </tr></thead>
            <tbody>
              ${list.map((c) => html`<tr>
                <td>${c.label || html`<span class="muted">–</span>`}</td>
                <td class="small mono">${c.auth_kind || '–'}</td>
                <td class="mono small">${(c.secret_keys || []).join(', ') || '–'}</td>
                <td class="num">${c.priority}</td>
                <td>
                  ${c.enabled
                    ? html`<span class="badge ok">启用</span>`
                    : html`<span class="badge">停用</span>`}
                  ${c.fail_until && c.fail_until * 1000 > Date.now()
                    ? html` <span class="badge warn">冷却至 ${fmtUnix(c.fail_until)}</span>` : null}
                </td>
                <td class="small muted">${fmtUnix(c.last_used_at)}</td>
                <td class="small wrap" style="max-width:260px">
                  ${c.last_error ? html`<span style="color:var(--err)">${c.last_error}</span>` : html`<span class="muted">–</span>`}
                </td>
                <td>
                  <div class="btnrow">
                    <button class="sm ghost" disabled=${busyId === c.id} onClick=${() => doVerify(c)}>
                      ${busyId === c.id ? '…' : '验证'}
                    </button>
                    ${canRefresh(c.auth_kind)
                      ? html`<button class="sm ghost" disabled=${busyId === c.id} onClick=${() => doRefresh(c)}>
                          ${busyId === c.id ? '…' : '刷新授权'}
                        </button>`
                      : null}
                    <button class="sm ghost" onClick=${() => doToggle(c)}>${c.enabled ? '停用' : '启用'}</button>
                    <button class="sm ghost" onClick=${() => setEditing({ mode: 'edit', row: c })}>编辑</button>
                    <button class="sm danger" onClick=${() => doDelete(c)}>删除</button>
                  </div>
                </td>
              </tr>`)}
            </tbody>
          </table></div>
        </div>`)}

    ${editing ? html`<${CredentialForm}
      providers=${providers}
      initial=${editing.mode === 'edit' ? editing.row : null}
      onClose=${() => setEditing(null)}
      onSaved=${() => { setEditing(null); creds.reload(); }}
    />` : null}
  </div>`;
}
