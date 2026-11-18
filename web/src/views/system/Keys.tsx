import { useEffect, useState } from "react";
import { Copy, KeyRound, RefreshCw, Trash2 } from "lucide-react";
import { createKey, listKeys, revokeKey, rotateKey } from "@/api/keys";
import type { APIKey, CreatedKey, KeyKind } from "@/api/types";
import { useI18n } from "@/i18n";
import { formatTime, relativeTime } from "@/lib/format";
import {
  Badge,
  Button,
  Card,
  Empty,
  ErrorBox,
  Field,
  Input,
  Loading,
  PageHeader,
  Select,
  Textarea,
} from "@/components/ui";

const KINDS: KeyKind[] = ["admin", "client", "agent", "tool"];

const KIND_STYLES: Record<KeyKind, string> = {
  admin: "bg-violet-100 text-violet-700 dark:bg-violet-500/15 dark:text-violet-300",
  client: "bg-sky-100 text-sky-700 dark:bg-sky-500/15 dark:text-sky-300",
  agent: "bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300",
  tool: "bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300",
};

export default function Keys() {
  const { t } = useI18n();
  const [keys, setKeys] = useState<APIKey[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [formOpen, setFormOpen] = useState(false);
  const [form, setForm] = useState<{ kind: KeyKind; name: string; notes: string; days: string }>({
    kind: "agent",
    name: "",
    notes: "",
    days: "",
  });
  const [created, setCreated] = useState<CreatedKey | null>(null);
  const [copied, setCopied] = useState(false);
  const [busy, setBusy] = useState("");

  async function load() {
    setError(null);
    try {
      const data = await listKeys();
      setKeys(data.keys ?? []);
    } catch (err) {
      setError(err);
    }
  }

  useEffect(() => {
    void load();
  }, []);

  async function submit() {
    setBusy("create");
    setError(null);
    try {
      const days = Number(form.days);
      const result = await createKey({
        kind: form.kind,
        name: form.name.trim() || undefined,
        notes: form.notes.trim() || undefined,
        expires_in_days: Number.isFinite(days) && days > 0 ? Math.floor(days) : undefined,
      });
      setCreated(result);
      setCopied(false);
      setFormOpen(false);
      setForm({ kind: "agent", name: "", notes: "", days: "" });
      await load();
    } catch (err) {
      setError(err);
    } finally {
      setBusy("");
    }
  }

  async function run(id: string, action: () => Promise<CreatedKey | void>) {
    setBusy(id);
    setError(null);
    try {
      const result = await action();
      if (result) {
        setCreated(result);
        setCopied(false);
      }
      await load();
    } catch (err) {
      setError(err);
    } finally {
      setBusy("");
    }
  }

  async function copyPlaintext() {
    if (!created) return;
    try {
      await navigator.clipboard.writeText(created.plaintext);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }

  return (
    <>
      <PageHeader
        title={t("keys.title")}
        description={t("keys.description")}
        actions={
          <Button onClick={() => setFormOpen((open) => !open)}>
            <KeyRound className="h-4 w-4" />
            {t("keys.create")}
          </Button>
        }
      />

      {error ? <ErrorBox error={error} /> : null}

      {created ? (
        <Card className="border-emerald-300 bg-emerald-50/60 dark:border-emerald-500/30 dark:bg-emerald-500/5">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <p className="text-sm font-medium">{t("keys.createdTitle")}</p>
              <p className="text-xs text-muted-foreground">{t("keys.oneTime")}</p>
            </div>
            <div className="flex gap-2">
              <Button variant="ghost" onClick={() => void copyPlaintext()}>
                <Copy className="h-4 w-4" />
                {copied ? t("common.copied") : t("common.copy")}
              </Button>
              <Button variant="ghost" onClick={() => setCreated(null)}>
                {t("keys.dismiss")}
              </Button>
            </div>
          </div>
          <code className="mt-3 block break-all rounded-lg bg-background/70 px-3 py-2 font-mono text-xs">
            {created.plaintext}
          </code>
        </Card>
      ) : null}

      {formOpen ? (
        <Card>
          <form
            className="grid gap-3 sm:grid-cols-2"
            onSubmit={(event) => {
              event.preventDefault();
              void submit();
            }}
          >
            <Field label={t("keys.kind")}>
              <Select
                value={form.kind}
                onChange={(event) => setForm({ ...form, kind: event.target.value as KeyKind })}
              >
                {KINDS.map((kind) => (
                  <option key={kind} value={kind}>
                    {t(`keys.kind.${kind}`)}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t("keys.name")}>
              <Input
                value={form.name}
                placeholder={t("keys.namePlaceholder")}
                onChange={(event) => setForm({ ...form, name: event.target.value })}
              />
            </Field>
            <Field label={t("keys.expiresDays")}>
              <Input
                type="number"
                min={0}
                value={form.days}
                placeholder={t("keys.expiresPlaceholder")}
                onChange={(event) => setForm({ ...form, days: event.target.value })}
              />
            </Field>
            <Field label={t("keys.notes")} className="sm:col-span-2">
              <Textarea
                rows={2}
                value={form.notes}
                onChange={(event) => setForm({ ...form, notes: event.target.value })}
              />
            </Field>
            <p className="text-xs text-muted-foreground sm:col-span-2">
              {t(`keys.kindHint.${form.kind}`)}
            </p>
            <div className="flex gap-2 sm:col-span-2">
              <Button type="submit" disabled={busy === "create"}>
                {busy === "create" ? t("keys.creating") : t("keys.create")}
              </Button>
              <Button type="button" variant="ghost" onClick={() => setFormOpen(false)}>
                {t("keys.cancel")}
              </Button>
            </div>
          </form>
        </Card>
      ) : null}

      <Card>
        {keys === null ? (
          <Loading />
        ) : keys.length === 0 ? (
          <Empty text={t("keys.empty")} />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">{t("keys.name")}</th>
                  <th>{t("keys.kind")}</th>
                  <th>{t("keys.prefix")}</th>
                  <th>{t("keys.scopes")}</th>
                  <th>{t("keys.createdAt")}</th>
                  <th>{t("keys.expiresAt")}</th>
                  <th>{t("keys.lastUsed")}</th>
                  <th>{t("common.status")}</th>
                  <th className="text-right">{t("common.none")}</th>
                </tr>
              </thead>
              <tbody>
                {keys.map((key) => (
                  <tr key={key.id} className="border-t">
                    <td className="py-2">
                      <div className="font-medium">{key.name}</div>
                      {key.notes ? (
                        <div className="max-w-[240px] truncate text-xs text-muted-foreground">
                          {key.notes}
                        </div>
                      ) : null}
                    </td>
                    <td>
                      <Badge className={KIND_STYLES[key.kind] ?? ""}>
                        {t(`keys.kind.${key.kind}`)}
                      </Badge>
                    </td>
                    <td className="font-mono text-xs text-muted-foreground">{key.prefix}…</td>
                    <td className="max-w-[180px] truncate text-xs text-muted-foreground">
                      {key.scopes?.join(", ") || t("common.none")}
                    </td>
                    <td className="text-xs text-muted-foreground">{formatTime(key.created_at)}</td>
                    <td className="text-xs text-muted-foreground">
                      {key.expires_at ? formatTime(key.expires_at) : t("keys.never")}
                    </td>
                    <td className="text-xs text-muted-foreground">
                      {key.last_used_at ? relativeTime(key.last_used_at) : t("keys.unused")}
                    </td>
                    <td>
                      <Badge
                        className={
                          key.revoked
                            ? "bg-slate-200 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300"
                            : "bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300"
                        }
                      >
                        {key.revoked ? t("keys.revoked") : t("keys.active")}
                      </Badge>
                    </td>
                    <td className="py-2 text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          disabled={key.revoked || busy === `rotate:${key.id}`}
                          onClick={() => {
                            if (!window.confirm(t("keys.rotateConfirm", { name: key.name }))) return;
                            void run(`rotate:${key.id}`, () => rotateKey(key.id));
                          }}
                        >
                          <RefreshCw className="h-3.5 w-3.5" />
                          {t("keys.rotate")}
                        </Button>
                        <Button
                          variant="danger"
                          disabled={key.revoked || busy === `revoke:${key.id}`}
                          onClick={() => {
                            if (!window.confirm(t("keys.revokeConfirm", { name: key.name }))) return;
                            void run(`revoke:${key.id}`, () => revokeKey(key.id));
                          }}
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                          {t("keys.revoke")}
                        </Button>
                      </div>
                    </td>
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
