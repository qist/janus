import { useEffect, useState } from "react";
import { listScores } from "@/api/insight";
import type { Score } from "@/api/types";
import { Card, Empty, ErrorBox, Loading, PageHeader, StatusBadge } from "@/components/ui";

function rate(part: number, total: number): string {
  if (!total) return "—";
  return `${Math.round((part / total) * 100)}%`;
}

export default function Scores() {
  const [scores, setScores] = useState<Score[] | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    listScores()
      .then((data) => setScores(data.scores))
      .catch(setError);
  }, []);

  return (
    <>
      <PageHeader
        title="模型评分"
        description="按 (agent, model, task_class, tool_mode, model_version) 分开计分，只用于准入。"
      />

      {error ? <ErrorBox error={error} /> : null}

      <Card>
        {scores === null ? (
          <Loading />
        ) : scores.length === 0 ? (
          <Empty text="还没有评分记录：需要先有评审结果。" />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">Agent / 模型</th>
                  <th>任务类</th>
                  <th>模式</th>
                  <th>等级</th>
                  <th className="text-right">分数</th>
                  <th className="text-right">样本</th>
                  <th className="text-right">验收</th>
                  <th className="text-right">一次通过</th>
                  <th className="text-right">连续 reject</th>
                </tr>
              </thead>
              <tbody>
                {scores.map((score) => (
                  <tr
                    key={`${score.key.agent_id}|${score.key.model_id}|${score.key.task_class}|${score.key.tool_mode}|${score.key.model_version}`}
                    className="border-t"
                  >
                    <td className="py-2">
                      <div className="font-medium">{score.key.model_id}</div>
                      <div className="text-xs text-muted-foreground">
                        {score.key.agent_id} · {score.key.model_version}
                      </div>
                    </td>
                    <td>{score.key.task_class}</td>
                    <td>{score.key.tool_mode}</td>
                    <td>
                      <StatusBadge status={score.tier} />
                    </td>
                    <td className="text-right font-medium">{score.score}</td>
                    <td className="text-right">{score.samples}</td>
                    <td className="text-right">{rate(score.approvals, score.samples)}</td>
                    <td className="text-right">{rate(score.first_pass, score.samples)}</td>
                    <td className="text-right">{score.reject_streak}</td>
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
