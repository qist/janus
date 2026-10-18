// util.js —— 格式化与剪贴板小工具。

export function fmtInt(n) {
  if (n === null || n === undefined || n === '') return '–';
  return Number(n).toLocaleString('en-US');
}

export function fmtCompact(n) {
  const v = Number(n || 0);
  if (Math.abs(v) >= 1e9) return (v / 1e9).toFixed(2) + 'B';
  if (Math.abs(v) >= 1e6) return (v / 1e6).toFixed(2) + 'M';
  if (Math.abs(v) >= 1e4) return (v / 1e3).toFixed(1) + 'K';
  return fmtInt(v);
}

export function fmtUSD(v, force) {
  const n = Number(v || 0);
  if (!force && n === 0) return '$0';
  if (Math.abs(n) < 0.01) return '$' + n.toFixed(4);
  return '$' + n.toFixed(2);
}

export function fmtPct(x, digits) {
  if (x === null || x === undefined || !isFinite(x)) return '–';
  return (Number(x) * 100).toFixed(digits === undefined ? 1 : digits) + '%';
}

export function fmtTime(iso) {
  if (!iso) return '–';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return String(iso);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export function fmtUnix(sec) {
  if (!sec) return '–';
  return fmtTime(new Date(Number(sec) * 1000).toISOString());
}

export function relTime(iso) {
  if (!iso) return '–';
  const t = new Date(iso).getTime();
  if (isNaN(t)) return String(iso);
  const diff = Math.floor((Date.now() - t) / 1000);
  if (diff < 0) return '刚刚';
  if (diff < 60) return diff + ' 秒前';
  if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
  return Math.floor(diff / 86400) + ' 天前';
}

export function shortDate(d) {
  return d && d.length >= 10 ? d.slice(5, 10) : d;
}

export function fmtDur(sec) {
  const s = Number(sec || 0);
  if (s <= 0) return '–';
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 86400) return (s / 3600).toFixed(1) + 'h';
  return (s / 86400).toFixed(1) + 'd';
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // 非安全上下文（http + 非 localhost）下 clipboard 不可用，退化成 execCommand。
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { ok = false; }
    ta.remove();
    return ok;
  }
}

export function jsonPretty(v) {
  try { return JSON.stringify(v, null, 2); } catch { return String(v); }
}
