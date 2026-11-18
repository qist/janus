import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { createItem, listItems } from "@/api/work";
import type { WorkItem } from "@/api/types";
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

const STATUSES = [
  "",
  "open",
  "running",
  "review",
  "approved",
  "integrating",
  "done",
  "blocked",
  "cancelled",
];

export default function Items() {
  const [status, setStatus] = useState("");
  const [items, setItems] = useState<WorkItem[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState({
    id: "",
    workspace_id: "ws",
    title: "",
    paths: "",
    risk: "medium",
    priority: "0",
    goal: "",
  });

  useEffect(() => {
    let cancelled = false;
    setItems(null);
    listItems({ status })
      .then((data) => {
        if (!cancelled) setItems(data.items);
      })
      .catch((err) => {
        if (!cancelled) setError(err);
      });
    return () => {
      cancelled = true;
    };
  }, [status]);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await createItem({
        id: form.id,
        workspace_id: form.workspace_id,
        title: form.title,
        priority: Number(form.priority) || 0,
        contract: {
          goal: form.goal,
          risk: form.risk,
          paths: form.paths
            .split(",")
            .map((part) => part.trim())
            .filter(Boolean),
        },
      } as WorkItem);
      setForm({ ...form, id: "", title: "", goal: "" });
      setStatus("");
      const data = await listItems({});
      setItems(data.items);
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <PageHeader
        title="工作项"
        description="工作板上的可认领单元；点进去看租约、证据与评审。"
        actions={
          <Select value={status} onChange={(event) => setStatus(event.target.value)} className="w-36">
            {STATUSES.map((value) => (
              <option key={value} value={value}>
                {value || "全部状态"}
              </option>
            ))}
          </Select>
        }
      />

      {error ? <ErrorBox error={error} /> : null}

      <Card>
        {items === null ? (
          <Loading />
        ) : items.length === 0 ? (
          <Empty text="该过滤条件下没有工作项。" />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">ID</th>
                  <th>标题</th>
                  <th>状态</th>
                  <th>风险</th>
                  <th className="text-right">优先级</th>
                  <th className="text-right">尝试</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <tr key={item.id} className="border-t">
                    <td className="py-2 font-mono text-xs">
                      <Link to={`/items/${item.id}`}>{item.id}</Link>
                    </td>
                    <td className="max-w-xs truncate">{item.title}</td>
                    <td>
                      <StatusBadge status={item.status} />
                    </td>
                    <td className="text-xs text-muted-foreground">{item.contract?.risk}</td>
                    <td className="text-right">{item.priority || 0}</td>
                    <td className="text-right">{item.attempts || 0}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <Card>
        <h2 className="mb-3 text-sm font-semibold">新建工作项</h2>
        <form onSubmit={submit} className="grid gap-3 sm:grid-cols-2">
          <Field label="ID">
            <Input
              required
              placeholder="w1"
              value={form.id}
              onChange={(event) => setForm({ ...form, id: event.target.value })}
            />
          </Field>
          <Field label="工作区">
            <Input
              required
              value={form.workspace_id}
              onChange={(event) => setForm({ ...form, workspace_id: event.target.value })}
            />
          </Field>
          <Field label="标题" className="sm:col-span-2">
            <Input
              required
              value={form.title}
              onChange={(event) => setForm({ ...form, title: event.target.value })}
            />
          </Field>
          <Field label="路径（逗号分隔）">
            <Input
              placeholder="internal/work"
              value={form.paths}
              onChange={(event) => setForm({ ...form, paths: event.target.value })}
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
          <Field label="优先级">
            <Input
              type="number"
              value={form.priority}
              onChange={(event) => setForm({ ...form, priority: event.target.value })}
            />
          </Field>
          <Field label="目标" className="sm:col-span-2">
            <Textarea
              rows={2}
              value={form.goal}
              onChange={(event) => setForm({ ...form, goal: event.target.value })}
            />
          </Field>
          <div className="sm:col-span-2">
            <Button type="submit" disabled={busy}>
              {busy ? "创建中…" : "创建工作项"}
            </Button>
          </div>
        </form>
      </Card>
    </>
  );
}
