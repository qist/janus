import { useEffect, useState } from "react";
import { getUsage } from "@/api/insight";
import type { UsageTotals } from "@/api/types";
import {
  Button,
  Card,
  ErrorBox,
  Field,
  Input,
  Loading,
  PageHeader,
} from "@/components/ui";

export default function Usage() {
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
      <PageHeader title="用量" description="本地账本聚合；cost_usd 由调用方按价格表写入。" />

      {error ? <ErrorBox error={error} /> : null}

      <Card>
        <form
          className="grid gap-3 sm:grid-cols-3 lg:grid-cols-5"
          onSubmit={(event) => {
            event.preventDefault();
            load();
          }}
        >
          <Field label="key / agent">
            <Input
              value={filter.key}
              onChange={(event) => setFilter({ ...filter, key: event.target.value })}
            />
          </Field>
          <Field label="provider">
            <Input
              value={filter.provider}
              onChange={(event) => setFilter({ ...filter, provider: event.target.value })}
            />
          </Field>
          <Field label="model">
            <Input
              value={filter.model}
              onChange={(event) => setFilter({ ...filter, model: event.target.value })}
            />
          </Field>
          <Field label="since（Unix 秒）">
            <Input
              type="number"
              value={filter.since}
              onChange={(event) => setFilter({ ...filter, since: event.target.value })}
            />
          </Field>
          <Field label="until（Unix 秒）">
            <Input
              type="number"
              value={filter.until}
              onChange={(event) => setFilter({ ...filter, until: event.target.value })}
            />
          </Field>
          <div className="sm:col-span-3 lg:col-span-5">
            <Button type="submit">应用过滤</Button>
          </div>
        </form>
      </Card>

      {totals === null ? (
        <Loading />
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <Card>
            <p className="label">调用次数</p>
            <p className="mt-1 text-2xl font-semibold">{totals.entries}</p>
          </Card>
          <Card>
            <p className="label">Prompt tokens</p>
            <p className="mt-1 text-2xl font-semibold">{totals.prompt_tokens.toLocaleString()}</p>
          </Card>
          <Card>
            <p className="label">Completion tokens</p>
            <p className="mt-1 text-2xl font-semibold">{totals.completion_tokens.toLocaleString()}</p>
          </Card>
          <Card>
            <p className="label">成本（USD）</p>
            <p className="mt-1 text-2xl font-semibold">{totals.cost_usd.toFixed(4)}</p>
            <p className="mt-1 text-xs text-muted-foreground">cached {totals.cached_tokens}</p>
          </Card>
        </div>
      )}
    </>
  );
}
