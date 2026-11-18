import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { listGoals } from "@/api/goals";
import { listItems } from "@/api/work";
import { getUsage, listScores } from "@/api/insight";
import type { Goal, Score, UsageTotals, WorkItem } from "@/api/types";
import { Card, ErrorBox, Loading, PageHeader, StatusBadge } from "@/components/ui";
import { formatTime, pct } from "@/lib/format";

type Data = {
  goals: Goal[];
  items: WorkItem[];
  usage: UsageTotals;
  scores: Score[];
};

export default function Dashboard() {
  const [data, setData] = useState<Data | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    let cancelled = false;
    Promise.all([listGoals(5), listItems(), getUsage(), listScores()])
      .then(([goals, items, usage, scores]) => {
        if (!cancelled) {
          setData({ goals: goals.goals, items: items.items, usage, scores: scores.scores });
        }
      })
      .catch((err) => {
        if (!cancelled) setError(err);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (error) return <ErrorBox error={error} />;
  if (!data) return <Loading />;

  const counts = data.items.reduce<Record<string, number>>((acc, item) => {
    acc[item.status] = (acc[item.status] || 0) + 1;
    return acc;
  }, {});
  const active = (counts.running || 0) + (counts.review || 0) + (counts.integrating || 0);
  const tokens = data.usage.prompt_tokens + data.usage.completion_tokens;
  const openGoals = data.goals.filter((goal) => goal.status !== "done" && goal.status !== "cancelled").length;

  return (
    <>
      <PageHeader title="概览" description="工作板、目标与模型侧的当前状态。" />

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Card>
          <p className="label">未完成目标</p>
          <p className="mt-1 text-2xl font-semibold">{openGoals}</p>
          <p className="mt-1 text-xs text-muted-foreground">最近 {data.goals.length} 个目标</p>
        </Card>
        <Card>
          <p className="label">进行中工作项</p>
          <p className="mt-1 text-2xl font-semibold">{active}</p>
          <p className="mt-1 text-xs text-muted-foreground">running / review / integrating</p>
        </Card>
        <Card>
          <p className="label">阻塞</p>
          <p className="mt-1 text-2xl font-semibold text-danger">{counts.blocked || 0}</p>
          <p className="mt-1 text-xs text-muted-foreground">需要人或强制派工</p>
        </Card>
        <Card>
          <p className="label">累计 token</p>
          <p className="mt-1 text-2xl font-semibold">{tokens.toLocaleString()}</p>
          <p className="mt-1 text-xs text-muted-foreground">{data.usage.entries} 次调用</p>
        </Card>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <div className="mb-3 flex items-center justify-between">
            <h2 className="text-sm font-semibold">最近目标</h2>
            <Link to="/goals" className="text-xs">
              全部
            </Link>
          </div>
          {data.goals.length === 0 ? (
            <p className="text-sm text-muted-foreground">还没有目标。</p>
          ) : (
            <ul className="divide-y">
              {data.goals.map((goal) => (
                <li key={goal.id} className="flex items-center justify-between gap-3 py-2">
                  <div className="min-w-0">
                    <Link to={`/goals/${goal.id}`} className="block truncate text-sm font-medium">
                      {goal.title}
                    </Link>
                    <p className="text-xs text-muted-foreground">{formatTime(goal.created_at)}</p>
                  </div>
                  <StatusBadge status={goal.status} />
                </li>
              ))}
            </ul>
          )}
        </Card>

        <Card>
          <h2 className="mb-3 text-sm font-semibold">工作项状态</h2>
          {Object.keys(counts).length === 0 ? (
            <p className="text-sm text-muted-foreground">工作板为空。</p>
          ) : (
            <ul className="space-y-2">
              {Object.entries(counts).map(([status, count]) => (
                <li key={status} className="flex items-center gap-3">
                  <span className="w-24">
                    <StatusBadge status={status} />
                  </span>
                  <div className="h-2 flex-1 overflow-hidden rounded-full bg-muted">
                    <div
                      className="h-full bg-primary"
                      style={{ width: `${pct((count / data.items.length) * 100)}%` }}
                    />
                  </div>
                  <span className="w-8 text-right text-sm">{count}</span>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>

      <Card>
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-sm font-semibold">模型评分</h2>
          <Link to="/scores" className="text-xs">
            全部
          </Link>
        </div>
        {data.scores.length === 0 ? (
          <p className="text-sm text-muted-foreground">还没有评分记录。</p>
        ) : (
          <table className="w-full text-sm">
            <thead className="text-left text-xs text-muted-foreground">
              <tr>
                <th className="py-2">模型</th>
                <th>等级</th>
                <th className="text-right">分数</th>
                <th className="text-right">样本</th>
              </tr>
            </thead>
            <tbody>
              {data.scores.slice(0, 5).map((score) => (
                <tr key={`${score.key.agent_id}-${score.key.model_id}`} className="border-t">
                  <td className="py-2">
                    <span className="font-medium">{score.key.model_id}</span>
                    <span className="ml-2 text-xs text-muted-foreground">{score.key.task_class}</span>
                  </td>
                  <td>
                    <StatusBadge status={score.tier} />
                  </td>
                  <td className="text-right">{score.score}</td>
                  <td className="text-right">{score.samples}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>
    </>
  );
}
