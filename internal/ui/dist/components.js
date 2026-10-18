// components.js —— 通用 UI 原语：Toast / Modal / Confirm / 表格 / 表单行。

import { html } from './html.js';
import { Fragment } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import { signal, useSignal } from './store.js';
import { copyText } from './util.js';

// ---------- Toast ----------
const toastList = signal([]);
let toastSeq = 0;

export function toast(msg, kind) {
  const id = ++toastSeq;
  toastList.value = [...toastList.value, { id, msg: String(msg), kind: kind || 'info' }];
  setTimeout(() => {
    toastList.value = toastList.value.filter((t) => t.id !== id);
  }, kind === 'err' ? 6000 : 3200);
}

export function Toasts() {
  const list = useSignal(toastList);
  return html`<div class="toasts">
    ${list.map((t) => html`<div class="toast ${t.kind}" key=${t.id}>${t.msg}</div>`)}
  </div>`;
}

// 把异常统一转成 toast。
export function toastErr(e) {
  toast(e && e.message ? e.message : String(e), 'err');
}

// ---------- Modal ----------
export function Modal({ title, onClose, children, footer, wide }) {
  useEffect(() => {
    const onKey = (e) => { if (e.key === 'Escape') onClose(); };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);
  return html`<div class="mask" onMouseDown=${(e) => { if (e.target === e.currentTarget) onClose(); }}>
    <div class="modal ${wide ? 'wide' : ''}" role="dialog" aria-modal="true">
      <div class="mhead">
        <h2>${title}</h2>
        <button class="x" onClick=${onClose} title="关闭">×</button>
      </div>
      <div class="mbody">${children}</div>
      ${footer ? html`<div class="mfoot">${footer}</div>` : null}
    </div>
  </div>`;
}

// ---------- 确认框 ----------
const confirmState = signal(null);

export function confirmDialog(opts) {
  return new Promise((resolve) => {
    confirmState.value = { ...opts, resolve };
  });
}

export function ConfirmHost() {
  const st = useSignal(confirmState);
  if (!st) return null;
  const done = (v) => { st.resolve(v); confirmState.value = null; };
  return html`<${Modal} title=${st.title || '确认'} onClose=${() => done(false)}
    footer=${html`<${Fragment}>
      <button onClick=${() => done(false)}>取消</button>
      <button class=${st.danger ? 'danger' : 'primary'} onClick=${() => done(true)}>${st.okText || '确定'}</button>
    <//>`}>
    <div style="font-size:13px;line-height:1.7">${st.message}</div>
  <//>`;
}

// ---------- 复制按钮 ----------
export function CopyBtn({ text, label, small }) {
  const [done, setDone] = useState(false);
  const click = async () => {
    const ok = await copyText(text);
    if (!ok) { toast('复制失败，请手动选择文本', 'warn'); return; }
    setDone(true);
    setTimeout(() => setDone(false), 1200);
  };
  return html`<button class=${'ghost ' + (small ? 'sm' : '')} onClick=${click} title="复制">
    ${done ? '已复制' : (label || '复制')}
  </button>`;
}

// ---------- 表单字段 ----------
export function Field({ label, hint, children }) {
  return html`<div class="field">
    ${label ? html`<label>${label}</label>` : null}
    ${children}
    ${hint ? html`<span class="hint">${hint}</span>` : null}
  </div>`;
}

export function TextInput({ value, onInput, ...rest }) {
  return html`<input value=${value == null ? '' : value} onInput=${(e) => onInput(e.target.value)} ...${rest} />`;
}

// ---------- 空状态 / 加载 ----------
export function Empty({ children }) {
  return html`<div class="empty">${children || '暂无数据'}</div>`;
}

export function Loading({ text }) {
  return html`<div class="empty">${text || '加载中…'}</div>`;
}

export function ErrorBox({ error, onRetry }) {
  if (!error) return null;
  return html`<div class="alert err">
    ${error.message || String(error)}
    ${onRetry ? html` <button class="sm ghost" onClick=${onRetry}>重试</button>` : null}
  </div>`;
}

// ---------- 数据加载 hook ----------
// useLoad(fn, deps) → { data, error, loading, reload }
export function useLoad(fn, deps) {
  const [state, setState] = useState({ data: null, error: null, loading: true });
  const [tick, setTick] = useState(0);
  const fnRef = useRef(fn);
  fnRef.current = fn;

  useEffect(() => {
    let alive = true;
    setState((s) => ({ ...s, loading: true, error: null }));
    Promise.resolve()
      .then(() => fnRef.current())
      .then((data) => { if (alive) setState({ data, error: null, loading: false }); })
      .catch((error) => { if (alive) setState({ data: null, error, loading: false }); });
    return () => { alive = false; };
  }, [...(deps || []), tick]);

  return { ...state, reload: () => setTick((t) => t + 1) };
}

// ---------- 开关 ----------
export function Toggle({ checked, onChange, label }) {
  return html`<label class="switch">
    <input type="checkbox" checked=${!!checked} onChange=${(e) => onChange(e.target.checked)} />
    ${label || ''}
  </label>`;
}
