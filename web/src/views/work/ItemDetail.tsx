import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { assignItem, cancelItem, getItem } from "@/api/work";
import type { CheckResult, ItemView, Role } from "@/api/types";
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
import { formatTime, shortID } from "@/lib/format";

function OutputTail({ checks }: { checks?: CheckResult[] }) {
  const { t } = useI18n();
  if (!checks || checks.length === 0) return null;
  return (
    <div className="mt-2 space-y-2">
      {checks.map((check) => (
        <div key={`${check.name}-${check.exit_code}`} className="rounded-lg border bg-muted/40 p-2">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium">
              {check.name} <span className="text-muted-foreground">{check.cmd}</span>
            </span>
            <span className={check.exit_code === 0 ? "text-emerald-600" : "text-danger"}>
              {t("item.exit")} {check.exit_code}
            </span>
          </div>
          {check.output_tail ? (
            <pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap break-all font-mono text-xs text-muted-foreground">
              {check.output_tail}
            </pre>
          ) : null}
        </div>
      ))}
    </div>
  );
}

export default function ItemDetail() {
  const { id = "" } = useParams();
  const { t } = useI18n();
  const [view, setView] = useState<ItemView | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [assign, setAssign] = useState({
    agent_id: "",
    role: "worker" as Role,
    plan: "",
    model_id: "",
  });

  function load() {
    getItem(id)
      .then(setView)
      .catch(setError);
  }

  useEffect(load, [id]);

  async function onCancel() {
    if (!window.confirm(t("item.cancelConfirm"))) return;
    setBusy(true);
    try {
      await cancelItem(id);
      load();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  async function onAssign(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await assignItem(id, assign);
      load();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  if (error) return <ErrorBox error={error} />;
  if (!view) return <Loading />;

  const { item, claim, reviews, warnings } = view;
  const terminal = item.status === "done" || item.status === "cancelled";
  const evidence = claim?.evidence;

  return (
    <>
      <PageHeader
        title={item.title}
        description={`${item.id} · ${item.workspace_id}`}
        actions={
          <>
            <Link to="/items" className="text-sm">
              {t("item.back")}
            </Link>
            <Button variant="danger" disabled={terminal || busy} onClick={onCancel}>
              {t("item.cancel")}
            </Button>
          </>
        }
      />

      {warnings && warnings.length > 0 ? (
        <div className="rounded-lg border border-amber-400/40 bg-amber-400/10 px-4 py-3 text-sm">
          {warnings.map((warning) => (
            <div key={warning}>{warning}</div>
          ))}
        </div>
      ) : null}

      <Card>
        <div className="flex flex-wrap items-center gap-3">
          <StatusBadge status={item.status} />
          <span className="text-xs text-muted-foreground">
            {t("common.risk")} {item.contract?.risk || "-"} · {t("common.priority")}{" "}
            {item.priority || 0} · {t("common.attempts")} {item.attempts || 0} · {t("goal.expired")}{" "}
            {item.lease_expiries || 0}
          </span>
        </div>
        <p className="mt-3 text-sm">{item.contract?.goal || item.brief || t("item.noGoal")}</p>
        <div className="mt-3 grid gap-2 text-xs text-muted-foreground sm:grid-cols-2">
          <div>
            {t("item.paths")}：{(item.contract?.paths || []).join(", ") || t("common.none")}
          </div>
          <div>
            {t("item.dependsOn")}：{(item.depends_on || []).join(", ") || t("common.none")}
          </div>
          <div>
            {t("item.created")}：{formatTime(item.created_at)}
          </div>
          <div>
            {t("item.createdBy")}：{item.created_by || t("common.none")}
          </div>
        </div>
      </Card>

      <Card>
        <h2 className="mb-2 text-sm font-semibold">{t("item.lease")}</h2>
        {claim ? (
          <>
            <div className="flex flex-wrap items-center gap-2 text-sm">
              <span className="font-medium">{claim.agent_id}</span>
              <span className="text-xs text-muted-foreground">
                {t.raw(`role.${claim.role}`, claim.role)} · {t("item.epoch")} {claim.lease_epoch} ·{" "}
                {t("item.expires")} {formatTime(claim.lease_expires_at)}
              </span>
              {claim.forced ? <StatusBadge status="blocked" /> : null}
            </div>
            <p className="mt-1 text-xs text-muted-foreground">{claim.plan}</p>
            {claim.model_id ? (
              <p className="mt-1 font-mono text-xs text-muted-foreground">
                {claim.model_id}
                {claim.model_version ? `@${claim.model_version}` : ""} ·{" "}
                {claim.tool_mode || "native"}
              </p>
            ) : null}

            <h3 className="mt-4 text-xs font-semibold text-muted-foreground">{t("item.evidence")}</h3>
            {evidence ? (
              <div className="mt-2 space-y-2 text-xs">
                {evidence.diff_hash ? (
                  <p className="font-mono text-muted-foreground">
                    {t("item.diff")} {shortID(evidence.diff_hash)}
                  </p>
                ) : null}
                {(evidence.artifacts || []).length > 0 ? (
                  <p className="text-muted-foreground">
                    {t("item.artifacts")}：{evidence.artifacts?.join(", ")}
                  </p>
                ) : null}
                <OutputTail checks={evidence.checks} />
              </div>
            ) : (
              <p className="mt-1 text-xs text-muted-foreground">{t("item.noEvidence")}</p>
            )}
          </>
        ) : (
          <Empty text={t("item.noLease")} />
        )}
      </Card>

      <Card>
        <h2 className="mb-3 text-sm font-semibold">{t("item.assign")}</h2>
        <form onSubmit={onAssign} className="grid gap-3 sm:grid-cols-2">
          <Field label={t("common.agent")}>
            <Input
              required
              value={assign.agent_id}
              onChange={(event) => setAssign({ ...assign, agent_id: event.target.value })}
            />
          </Field>
          <Field label={t("common.role")}>
            <Select
              value={assign.role}
              onChange={(event) => setAssign({ ...assign, role: event.target.value as Role })}
            >
              <option value="worker">{t.raw("role.worker", "worker")}</option>
              <option value="verifier">{t.raw("role.verifier", "verifier")}</option>
              <option value="integrator">{t.raw("role.integrator", "integrator")}</option>
            </Select>
          </Field>
          <Field label={t("common.model")}>
            <Input
              placeholder="model-1"
              value={assign.model_id}
              onChange={(event) => setAssign({ ...assign, model_id: event.target.value })}
            />
          </Field>
          <Field label={t("common.plan")} className="sm:col-span-2">
            <Textarea
              required
              rows={2}
              placeholder={t("item.assignPlan")}
              value={assign.plan}
              onChange={(event) => setAssign({ ...assign, plan: event.target.value })}
            />
          </Field>
          <div className="sm:col-span-2">
            <Button type="submit" variant="danger" disabled={busy}>
              {busy ? t("item.assigning") : t("item.assignSubmit")}
            </Button>
          </div>
        </form>
      </Card>

      <Card>
        <h2 className="mb-3 text-sm font-semibold">
          {t("item.reviews", { count: reviews?.length || 0 })}
        </h2>
        {!reviews || reviews.length === 0 ? (
          <Empty text={t("item.noReviews")} />
        ) : (
          <div className="space-y-3">
            {reviews.map((review) => (
              <div key={review.id} className="rounded-lg border p-3">
                <div className="flex flex-wrap items-center gap-2">
                  <StatusBadge status={review.verdict} />
                  <span className="text-xs text-muted-foreground">
                    {review.reviewer_id} · {formatTime(review.created_at)}
                  </span>
                </div>
                {review.reasons ? <p className="mt-2 text-sm">{review.reasons}</p> : null}
                <OutputTail checks={review.checks} />
              </div>
            ))}
          </div>
        )}
      </Card>
    </>
  );
}
