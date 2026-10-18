// store.js —— 极简信号（约 50 行），避免引入额外依赖。
//
// signal(v)       → 可读写：s.value / s.value = x
// computed(fn)    → 只读，依赖变化时重算
// effect(fn)      → 依赖变化时重跑（返回 dispose）
// 组件里用 useSignal(s) 订阅。

import { useEffect, useReducer } from 'preact/hooks';

let currentEffect = null;

function track(sig) {
  if (currentEffect) currentEffect.deps.add(sig);
}

export function signal(value) {
  const subs = new Set();
  const s = {
    _v: value,
    get value() { track(s); return s._v; },
    set value(v) {
      if (Object.is(v, s._v)) return;
      s._v = v;
      for (const fn of [...subs]) fn();
    },
    subscribe(fn) { subs.add(fn); return () => subs.delete(fn); },
  };
  return s;
}

export function computed(fn) {
  const out = signal(undefined);
  let dirty = true;
  let dispose = null;
  const compute = () => {
    if (dispose) dispose();
    currentEffect = { deps: new Set() };
    const prev = currentEffect;
    const v = fn();
    dispose = () => { for (const s of prev.deps) s.subscribe(compute); };
    // 依赖里已经包含 out 自身时要防止循环：这里直接重算并写值。
    currentEffect = null;
    out._v = v;
    dirty = false;
    for (const s of prev.deps) s.subscribe(() => { dirty = true; compute(); });
  };
  compute();
  return { get value() { if (dirty) compute(); return out._v; }, subscribe: out.subscribe };
}

export function effect(fn) {
  const prev = currentEffect;
  const deps = new Set();
  currentEffect = { deps };
  const cleanup = fn();
  currentEffect = prev;
  const subs = [];
  for (const s of deps) subs.push(s.subscribe(run));
  function run() {
    if (typeof cleanup === 'function') cleanup();
    const p = currentEffect;
    const d = new Set();
    currentEffect = { deps: d };
    fn();
    currentEffect = p;
    for (const s of d) if (!deps.has(s)) { deps.add(s); subs.push(s.subscribe(run)); }
  }
  return () => { for (const un of subs) un(); if (typeof cleanup === 'function') cleanup(); };
}

// useSignal 订阅一个信号，变化时触发组件重渲染。
export function useSignal(sig) {
  const [, force] = useReducer((x) => x + 1, 0);
  useEffect(() => sig.subscribe(force), [sig]);
  return sig.value;
}
