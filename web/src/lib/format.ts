export function formatTime(unix?: number): string {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString();
}

export function relativeTime(unix?: number): string {
  if (!unix) return "—";
  const diff = Math.floor(Date.now() / 1000) - unix;
  if (diff < 60) return `${Math.max(diff, 0)} 秒前`;
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
}

export function pct(value?: number): number {
  return Math.max(0, Math.min(100, Number(value) || 0));
}

export function shortID(id?: string): string {
  if (!id) return "—";
  return id.length > 18 ? `${id.slice(0, 12)}…` : id;
}

const STATUS_STYLES: Record<string, string> = {
  open: "bg-blue-100 text-blue-700 dark:bg-blue-500/15 dark:text-blue-300",
  running: "bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300",
  review: "bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300",
  approved: "bg-indigo-100 text-indigo-700 dark:bg-indigo-500/15 dark:text-indigo-300",
  integrating: "bg-violet-100 text-violet-700 dark:bg-violet-500/15 dark:text-violet-300",
  done: "bg-green-100 text-green-700 dark:bg-green-500/15 dark:text-green-300",
  blocked: "bg-red-100 text-red-700 dark:bg-red-500/15 dark:text-red-300",
  cancelled: "bg-slate-200 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300",
  planning: "bg-sky-100 text-sky-700 dark:bg-sky-500/15 dark:text-sky-300",
  active: "bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300",
  failed: "bg-red-100 text-red-700 dark:bg-red-500/15 dark:text-red-300",
  probation: "bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300",
  trusted: "bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300",
  suspended: "bg-red-100 text-red-700 dark:bg-red-500/15 dark:text-red-300",
  retired: "bg-slate-200 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300",
  approve: "bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300",
  reject: "bg-red-100 text-red-700 dark:bg-red-500/15 dark:text-red-300",
  needs_human: "bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300",
};

export function statusClass(status?: string): string {
  return STATUS_STYLES[status || ""] || "bg-muted text-muted-foreground";
}
