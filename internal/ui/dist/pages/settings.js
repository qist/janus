// pages/settings.js —— 运行期设置：表单由 /settings/schema 反射生成。
//
// 后端只有 GET/PATCH 两个端点，PATCH 收 {key: value} 后整份合并进
// settings.runtime 并立即 ApplySettings，所以前端只做「读值 → 编辑 → 提交」。

import { html } from '../html.js';
import { useMemo, useState } from 'preact/hooks';
import { api } from '../api.js';
import { useLoad, Loading, ErrorBox, Field, toast, toastErr, Toggle } from '../components.js';
import { jsonPretty } from '../util.js';

const GROUP_LABEL = {
  general: '通用',
  runtime: '运行期',
  upstream: '上游',
  security: '安全 / 限流',
  protocol: '协议兼容',
  tools: '工具调用',
  usage: '用量统计',
};

const GROUP_ORDER = ['general', 'runtime', 'upstream', 'security', 'protocol', 'tools', 'usage'];

// 这些键含敏感信息，用密码框显示。
const SECRET_KEYS = new Set(['password']);

// 值 → 表单文本。
function toText(item, v) {
  if (v === undefined || v === null) return '';
  if (item.type === 'list' || item.type === 'map') return jsonPretty(v);
  if (item.type === 'bool') return v ? 'true' : 'false';
  return String(v);
}

// 表单文本 → 提交值；解析失败抛错。
function fromText(item, text) {
  if (item.type === 'bool') return text === 'true';
  if (item.type === 'int') {
    const n = Number(text);
    if (!Number.isFinite(n)) throw new Error(item.key + ' 需要整数');
    return Math.trunc(n);
  }
  if (item.type === 'number') {
    const n = Number(text);
    if (!Number.isFinite(n)) throw new Error(item.key + ' 需要数字');
    return n;
  }
  if (item.type === 'list' || item.type === 'map') {
    try {
      return JSON.parse(text || (item.type === 'list' ? '[]' : '{}'));
    } catch (err) {
      throw new Error(item.key + ' 不是合法 JSON：' + err.message);
    }
  }
  return text;
}

function SettingControl({ item, text, onChange }) {
  if (item.type === 'bool') {
    return html`<${Toggle} checked=${text === 'true'} onChange=${(v) => onChange(v ? 'true' : 'false')} label=${text === 'true' ? '开启' : '关闭'} />`;
  }
  if (item.enum && item.enum.length) {
    return html`<select value=${text} onChange=${(e) => onChange(e.target.value)}>
      ${item.enum.map((v) => html`<option value=${v}>${v}</option>`)}
    </select>`;
  }
  if (item.type === 'int' || item.type === 'number') {
    return html`<input type="number" value=${text} onInput=${(e) => onChange(e.target.value)} />`;
  }
  if (item.type === 'duration') {
    return html`<input value=${text} onInput=${(e) => onChange(e.target.value)} placeholder="30s / 5m / never" spellcheck="false" />`;
  }
  if (item.type === 'list' || item.type === 'map') {
    return html`<textarea value=${text} onInput=${(e) => onChange(e.target.value)}
      placeholder=${item.type === 'list' ? '[]' : '{}'} spellcheck="false" style="min-height:74px" />`;
  }
  return html`<input type=${SECRET_KEYS.has(item.key) ? 'password' : 'text'} value=${text}
    onInput=${(e) => onChange(e.target.value)} autocomplete="off" spellcheck="false" />`;
}

function shortDefault(v) {
  const s = typeof v === 'string' ? v : JSON.stringify(v);
  if (s === undefined) return '';
  return s.length > 36 ? s.slice(0, 36) + '…' : s;
}

function SettingRow({ item, text, onChange }) {
  return html`<div class="srow">
    <div class="slabel">
      <code>${item.key}</code>
      <span class="small muted">
        ${item.type}${item.default !== undefined && item.type !== 'map' && item.type !== 'list'
          ? ' · 默认 ' + shortDefault(item.default) : ''}
      </span>
    </div>
    <div class="sctl"><${SettingControl} item=${item} text=${text} onChange=${onChange} /></div>
  </div>`;
}

export default function Settings() {
  const schema = useLoad(() => api.schema(), []);
  const current = useLoad(() => api.settings(), []);
  const [draft, setDraft] = useState({});
  const [busy, setBusy] = useState(false);

  const items = (schema.data && schema.data.settings) || [];
  const runtime = (current.data && current.data.runtime) || {};
  const defaultModel = (current.data && current.data.default_model) || '';

  const groups = useMemo(() => {
    const m = new Map();
    for (const it of items) {
      const g = it.group || 'general';
      if (!m.has(g)) m.set(g, []);
      m.get(g).push(it);
    }
    const rank = (g) => {
      const i = GROUP_ORDER.indexOf(g);
      return i < 0 ? 99 : i;
    };
    return [...m.entries()].sort((a, b) => rank(a[0]) - rank(b[0]));
  }, [items]);

  const textOf = (item) => (item.key in draft ? draft[item.key] : toText(item, runtime[item.key]));
  const dirtyKeys = Object.keys(draft);

  const save = async () => {
    if (!dirtyKeys.length) { toast('没有改动', 'warn'); return; }
    const patch = {};
    try {
      for (const key of dirtyKeys) {
        if (key === 'default_model') { patch[key] = draft[key]; continue; }
        const item = items.find((i) => i.key === key);
        if (!item) continue;
        patch[key] = fromText(item, String(draft[key]));
      }
    } catch (err) {
      toast(err.message, 'err');
      return;
    }
    setBusy(true);
    try {
      await api.patchSettings(patch);
      toast('已保存并生效', 'ok');
      setDraft({});
      current.reload();
    } catch (err) { toastErr(err); }
    finally { setBusy(false); }
  };

  const err = schema.error || current.error;

  return html`<div>
    <div class="pagehead">
      <div>
        <h1>设置</h1>
        <div class="sub">运行期配置存在数据库，保存后立即生效；bootstrap 项只能通过环境变量设置。</div>
      </div>
      <div class="actions">
        <button class="ghost" onClick=${() => { schema.reload(); current.reload(); setDraft({}); }}>重新加载</button>
        <button class="ghost" disabled=${!dirtyKeys.length || busy} onClick=${() => setDraft({})}>撤销改动</button>
        <button class="primary" disabled=${!dirtyKeys.length || busy} onClick=${save}>
          ${busy ? '保存中…' : (dirtyKeys.length ? '保存 ' + dirtyKeys.length + ' 项' : '保存')}
        </button>
      </div>
    </div>

    <${ErrorBox} error=${err} onRetry=${() => { schema.reload(); current.reload(); }} />

    ${dirtyKeys.length ? html`<div class="alert">
      已修改 ${dirtyKeys.length} 项（未保存）：<span class="mono small">${dirtyKeys.join(', ')}</span>
    </div>` : null}

    ${schema.loading || current.loading ? html`<${Loading} />`
      : items.length === 0 ? html`<div class="card">设置 schema 为空</div>`
      : html`<div>
          <div class="card">
            <div class="cardhead"><h2>默认模型</h2></div>
            <${Field} label="default_model" hint="客户端不指定模型时使用；留空则用 runtime 的默认值">
              <input value=${'default_model' in draft ? draft.default_model : defaultModel}
                onInput=${(e) => setDraft((d) => ({ ...d, default_model: e.target.value }))}
                placeholder="provider/model" spellcheck="false" />
            <//>
          </div>

          ${groups.map(([g, list]) => html`<div class="card" key=${g}>
            <div class="cardhead"><h2>${GROUP_LABEL[g] || g}</h2><span class="badge">${list.length}</span></div>
            ${list.map((item) => html`<${SettingRow} key=${item.key} item=${item}
              text=${textOf(item)}
              onChange=${(v) => setDraft((d) => ({ ...d, [item.key]: v }))} />`)}
          </div>`)}
        </div>`}
  </div>`;
}
