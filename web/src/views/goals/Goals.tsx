import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { createGoal, listGoals } from "@/api/goals";
import type { Goal } from "@/api/types";
import { useI18n } from "@/i18n";
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
  const { t } = useI18n();
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
      <PageHeader title={t("goals.title")} description={t("goals.description")} />

      <Card>
        <h2 className="mb-3 text-sm font-semibold">{t("goals.new")}</h2>
        <form onSubmit={submit} className="grid gap-3 sm:grid-cols-2">
          <Field label={t("goals.titleField")}>
            <Input
              required
              placeholder={t("goals.titlePlaceholder")}
              value={form.title}
              onChange={(event) => setForm({ ...form, title: event.target.value })}
            />
          </Field>
          <Field label={t("goals.workspace")}>
            <Input
              required
              value={form.workspace_id}
              onChange={(event) => setForm({ ...form, workspace_id: event.target.value })}
            />
          </Field>
          <Field label={t("goals.brief")} className="sm:col-span-2">
            <Textarea
              rows={3}
              placeholder={t("goals.briefPlaceholder")}
              value={form.brief}
              onChange={(event) => setForm({ ...form, brief: event.target.value })}
            />
          </Field>
          <Field label={t("goals.risk")}>
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
          <Field label={t("goals.budget")}>
            <Input
              type="number"
              min={0}
              value={form.budget_tokens}
              onChange={(event) => setForm({ ...form, budget_tokens: event.target.value })}
            />
          </Field>
          <div className="sm:col-span-2">
            <Button type="submit" disabled={busy}>
              {busy ? t("goals.submitting") : t("goals.submit")}
            </Button>
          </div>
        </form>
        {error ? (
          <div className="mt-3">
            <ErrorBox error={error} />
          </div>
        ) : null}
      </Card>

      <Card>
        <h2 className="mb-3 text-sm font-semibold">{t("goals.all")}</h2>
        {goals === null ? (
          <Loading />
        ) : goals.length === 0 ? (
          <Empty text={t("goals.empty")} />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">{t("goals.title")}</th>
                  <th>{t("common.status")}</th>
                  <th>{t("goals.items")}</th>
                  <th>{t("goals.createdAt")}</th>
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
