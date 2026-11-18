import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { createGoal, listGoals } from "@/api/goals";
import type { Goal } from "@/api/types";
import {
  Button,
  Card,
  Empty,
  ErrorBox,
  Field,
  Input,
  Loading,
  PageHeader,
  Select,
  StatusBadge,
  Textarea,
} from "@/components/ui";
import { formatTime } from "@/lib/format";

export default function Goals() {
  const [goals, setGoals] = useState<Goal[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({
    title: "",
    brief: "",
    workspace_id: "ws",
    risk: "medium",
    budget_tokens: "",
  });
  const navigate = useNavigate();

  function load() {
    listGoals(100)
      .then((data) => setGoals(data.goals))
      .catch(setError);
  }

  useEffect(load, []);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const created = await createGoal({
        title: form.title,
        brief: form.brief,
        workspace_id: form.workspace_id,
        risk: form.risk,
        budget_tokens: Number(form.budget_tokens) || undefined,
      });
      navigate(`/goals/${created.goal_id}`);
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <PageHeader title="目标" description="一段意图 → 后台拆项 → 工作板执行。" />

      <Card>
        <h2 className="mb-3 text-sm font-semibold">新建目标</h2>
        <form onSubmit={submit} className="grid gap-3 sm:grid-cols-2">
          <Field label="标题">
            <Input
              required
              placeholder="fix claim race"
              value={form.title}
              onChange={(event) => setForm({ ...form, title: event.target.value })}
            />
          </Field>
          <Field label="工作区">
            <Input
              required
              value={form.workspace_id}
              onChange={(event) => setForm({ ...form, workspace_id: event.target.value })}
            />
          </Field>
          <Field label="意图" className="sm:col-span-2">
            <Textarea
              rows={3}
              placeholder="一段话说明要达成什么，planner 会据此拆项"
              value={form.brief}
              onChange={(event) => setForm({ ...form, brief: event.target.value })}
            />
          </Field>
          <Field label="风险">
            <Select
              value={form.risk}
              onChange={(event) => setForm({ ...form, risk: event.target.value })}
            >
              <option value="low">low</option>
              <option value="medium">medium</option>
              <option value="high">high</option>
              <option value="critical">critical</option>
            </Select>
          </Field>
          <Field label="预算 token（可选）">
            <Input
              type="number"
              min={0}
              value={form.budget_tokens}
              onChange={(event) => setForm({ ...form, budget_tokens: event.target.value })}
            />
          </Field>
          <div className="sm:col-span-2">
            <Button type="submit" disabled={busy}>
              {busy ? "提交中…" : "提交（202 异步拆项）"}
            </Button>
          </div>
        </form>
        {error ? <div className="mt-3"><ErrorBox error={error} /></div> : null}
      </Card>

      <Card>
        <h2 className="mb-3 text-sm font-semibold">全部目标</h2>
        {goals === null ? (
          <Loading />
        ) : goals.length === 0 ? (
          <Empty text="还没有目标。" />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">目标</th>
                  <th>状态</th>
                  <th>工作项</th>
                  <th>创建时间</th>
                </tr>
              </thead>
              <tbody>
                {goals.map((goal) => (
                  <tr key={goal.id} className="border-t">
                    <td className="py-2">
                      <Link to={`/goals/${goal.id}`} className="font-medium">
                        {goal.title}
                      </Link>
                      <div className="font-mono text-xs text-muted-foreground">{goal.id}</div>
                    </td>
                    <td>
                      <StatusBadge status={goal.status} />
                    </td>
                    <td>{(goal.item_ids || []).length}</td>
                    <td className="text-xs text-muted-foreground">{formatTime(goal.created_at)}</td>
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
