import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { cancelGoal, getGoal } from "@/api/goals";
import { listItems } from "@/api/work";
import type { Goal, GoalProgress, WorkItem } from "@/api/types";
import {
  Button,
  Card,
  Empty,
  ErrorBox,
  Loading,
  PageHeader,
  StatusBadge,
} from "@/components/ui";
import { formatTime, pct } from "@/lib/format";

type Data = { goal: Goal; progress: GoalProgress; items: WorkItem[] };

const ACTIVE = new Set(["planning", "active", "blocked"]);

export default function GoalDetail() {
  const { id = "" } = useParams();
  const [data, setData] = useState<Data | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;

    async function load() {
      try {
        const [detail, list] = await Promise.all([getGoal(id), listItems()]);
        if (cancelled) return;
        const ids = new Set(detail.goal.item_ids || []);
        setData({
          goal: detail.goal,
          progress: detail.progress,
          items: list.items.filter((item) => ids.has(item.id)),
        });
        if (ACTIVE.has(detail.goal.status)) {
          timer = window.setTimeout(load, 2000);
        }
      } catch (err) {
        if (!cancelled) setError(err);
      }
    }

    load();
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
    };
  }, [id]);

  async function cancel() {
    setBusy(true);
    try {
      await cancelGoal(id);
      const detail = await getGoal(id);
      setData((current) => (current ? { ...current, goal: detail.goal, progress: detail.progress } : current));
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  if (error) return <ErrorBox error={error} />;
  if (!data) return <Loading />;

  const { goal, progress, items } = data;
  const terminal = goal.status === "done" || goal.status === "cancelled";

  return (
    <>
      <PageHeader
        title={goal.title}
        description={goal.brief || goal.id}
        actions={
          <>
            <Link to="/goals" className="text-sm">
              返回
            </Link>
            <Button variant="danger" disabled={terminal || busy} onClick={cancel}>
              {busy ? "取消中…" : "取消目标"}
            </Button>
          </>
        }
      />

      <Card>
        <div className="mb-2 flex items-center justify-between text-sm">
          <div className="flex items-center gap-2">
            <StatusBadge status={progress.status} />
            <span className="text-muted-foreground">
              完成 {progress.done}/{progress.total}
            </span>
          </div>
          <span className="font-medium">{pct(progress.percent)}%</span>
        </div>
        <div className="h-2 overflow-hidden rounded-full bg-muted">
          <div className="h-full bg-primary transition-all" style={{ width: `${pct(progress.percent)}%` }} />
        </div>
        <div className="mt-3 flex flex-wrap gap-2">
          {Object.entries(progress.counts || {}).map(([status, count]) => (
            <span key={status} className="flex items-center gap-1 text-xs">
              <StatusBadge status={status} />
              <span className="text-muted-foreground">×{count}</span>
            </span>
          ))}
        </div>
        {goal.error ? <p className="mt-3 text-sm text-danger">{goal.error}</p> : null}
        <p className="mt-3 text-xs text-muted-foreground">
          工作区 {goal.workspace_id} · 风险 {goal.risk || "medium"} · 创建 {formatTime(goal.created_at)}
        </p>
      </Card>

      <Card>
        <h2 className="mb-3 text-sm font-semibold">工作项</h2>
        {items.length === 0 ? (
          <Empty text={goal.status === "planning" ? "规划中…" : "没有工作项。"} />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">ID</th>
                  <th>标题</th>
                  <th>状态</th>
                  <th>尝试</th>
                  <th>租约过期</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <tr key={item.id} className="border-t">
                    <td className="py-2 font-mono text-xs">
                      <Link to={`/items/${item.id}`}>{item.id}</Link>
                    </td>
                    <td>{item.title}</td>
                    <td>
                      <StatusBadge status={item.status} />
                    </td>
                    <td>{item.attempts || 0}</td>
                    <td>{item.lease_expiries || 0}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  );
}
