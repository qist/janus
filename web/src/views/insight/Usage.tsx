import { useEffect, useState } from "react";
import { getUsage } from "@/api/insight";
import type { UsageTotals } from "@/api/types";
import { useI18n } from "@/i18n";
import { Button, Card, ErrorBox, Field, Input, Loading, PageHeader } from "@/components/ui";

export default function Usage() {
  const { t } = useI18n();
  const [totals, setTotals] = useState<UsageTotals | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [filter, setFilter] = useState({ provider: "", model: "", key: "", since: "", until: "" });

  function load() {
    setError(null);
    getUsage({
      provider: filter.provider || undefined,
      model: filter.model || undefined,
      key: filter.key || undefined,
      since: Number(filter.since) || undefined,
      until: Number(filter.until) || undefined,
    })
      .then(setTotals)
      .catch(setError);
  }

  useEffect(load, []);

  return (
    <>
      <PageHeader title={t("usage.title")} description={t("usage.description")} />

      {error ? <ErrorBox error={error} /> : null}

      <Card>
        <form
          className="grid gap-3 sm:grid-cols-3 lg:grid-cols-5"
          onSubmit={(event) => {
            event.preventDefault();
            load();
          }}
        >
          <Field label={t("usage.key")}>
            <Input
              value={filter.key}
              onChange={(event) => setFilter({ ...filter, key: event.target.value })}
            />
          </Field>
          <Field label={t("usage.provider")}>
            <Input
              value={filter.provider}
              onChange={(event) => setFilter({ ...filter, provider: event.target.value })}
            />
          </Field>
          <Field label={t("usage.model")}>
            <Input
              value={filter.model}
              onChange={(event) => setFilter({ ...filter, model: event.target.value })}
            />
          </Field>
          <Field label={t("usage.since")}>
            <Input
              type="number"
              value={filter.since}
              onChange={(event) => setFilter({ ...filter, since: event.target.value })}
            />
          </Field>
          <Field label={t("usage.until")}>
            <Input
              type="number"
              value={filter.until}
              onChange={(event) => setFilter({ ...filter, until: event.target.value })}
            />
          </Field>
          <div className="sm:col-span-3 lg:col-span-5">
            <Button type="submit">{t("usage.apply")}</Button>
          </div>
        </form>
      </Card>

      {totals === null ? (
        <Loading />
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <Card>
            <p className="label">{t("usage.calls")}</p>
            <p className="mt-1 text-2xl font-semibold">{totals.entries}</p>
          </Card>
          <Card>
            <p className="label">{t("usage.prompt")}</p>
            <p className="mt-1 text-2xl font-semibold">{totals.prompt_tokens.toLocaleString()}</p>
          </Card>
          <Card>
            <p className="label">{t("usage.completion")}</p>
            <p className="mt-1 text-2xl font-semibold">
              {totals.completion_tokens.toLocaleString()}
            </p>
          </Card>
          <Card>
            <p className="label">{t("usage.cost")}</p>
            <p className="mt-1 text-2xl font-semibold">{totals.cost_usd.toFixed(4)}</p>
            <p className="mt-1 text-xs text-muted-foreground">
              {t("usage.cached", { count: totals.cached_tokens })}
            </p>
          </Card>
        </div>
      )}
    </>
  );
}
