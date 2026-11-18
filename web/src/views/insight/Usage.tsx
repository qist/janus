import { useEffect, useState } from "react";
import { getUsageLogs, getUsageStats } from "@/api/insight";
import type { UsageEntry, UsageStats } from "@/api/types";
import { useI18n } from "@/i18n";
import { Button, Card, Empty, ErrorBox, Field, Input, Loading, PageHeader } from "@/components/ui";

type Period = "24h" | "7d" | "30d";

function formatTime(unix: number) {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString();
}

function formatTokens(value: number) {
  return value.toLocaleString();
}

export default function Usage() {
  const { t } = useI18n();
  const [period, setPeriod] = useState<Period>("7d");
  const [filter, setFilter] = useState({ provider: "", model: "", key: "" });
  const [stats, setStats] = useState<UsageStats | null>(null);
  const [entries, setEntries] = useState<UsageEntry[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(false);

  async function load(next: Period = period) {
    setLoading(true);
    setError(null);
    try {
      const query = {
        period: next,
        provider: filter.provider || undefined,
        model: filter.model || undefined,
        key: filter.key || undefined,
      };
      const [statsData, logData] = await Promise.all([
        getUsageStats(query),
        getUsageLogs({ ...query, limit: 50 }),
      ]);
      setStats(statsData);
      setEntries(logData.entries);
    } catch (err) {
      setError(err);
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    void load("7d");
    // The first render loads the default period; filters apply on submit.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const totals = stats?.totals;
  const trendMax = Math.max(1, ...(stats?.trend ?? []).map((point) => point.prompt_tokens + point.completion_tokens));

  return (
    <>
      <PageHeader title={t("usage.title")} description={t("usage.description")} />

      {error ? <ErrorBox error={error} /> : null}

      <Card>
        <div className="mb-3 flex flex-wrap gap-2">
          {(["24h", "7d", "30d"] as Period[]).map((item) => (
            <Button
              key={item}
              variant={period === item ? "default" : "ghost"}
              onClick={() => {
                setPeriod(item);
                void load(item);
              }}
            >
              {t(`usage.period.${item}`)}
            </Button>
          ))}
        </div>
        <form
          className="grid gap-3 sm:grid-cols-3 lg:grid-cols-4"
          onSubmit={(event) => {
            event.preventDefault();
            void load();
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
          <div className="flex items-end">
            <Button type="submit" disabled={loading}>
              {t("usage.apply")}
            </Button>
          </div>
        </form>
      </Card>

      {loading && !stats ? (
        <Loading />
      ) : totals ? (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-5">
          <Card>
            <p className="label">{t("usage.calls")}</p>
            <p className="mt-1 text-2xl font-semibold">{totals.entries}</p>
          </Card>
          <Card>
            <p className="label">{t("usage.failed")}</p>
            <p className="mt-1 text-2xl font-semibold">{totals.failed ?? 0}</p>
          </Card>
          <Card>
            <p className="label">{t("usage.prompt")}</p>
            <p className="mt-1 text-2xl font-semibold">{formatTokens(totals.prompt_tokens)}</p>
          </Card>
          <Card>
            <p className="label">{t("usage.completion")}</p>
            <p className="mt-1 text-2xl font-semibold">{formatTokens(totals.completion_tokens)}</p>
          </Card>
          <Card>
            <p className="label">{t("usage.cost")}</p>
            <p className="mt-1 text-2xl font-semibold">{totals.cost_usd.toFixed(4)}</p>
            <p className="mt-1 text-xs text-muted-foreground">
              {t("usage.cached", { count: totals.cached_tokens })}
            </p>
          </Card>
        </div>
      ) : null}

      {stats && stats.trend.length > 0 ? (
        <Card>
          <h2 className="mb-3 text-sm font-semibold">{t("usage.trend")}</h2>
          <div className="space-y-1.5">
            {stats.trend.map((point) => {
              const tokens = point.prompt_tokens + point.completion_tokens;
              return (
                <div key={point.start} className="flex items-center gap-3 text-xs">
                  <span className="w-36 shrink-0 text-muted-foreground">
                    {formatTime(point.start)}
                  </span>
                  <div className="h-2 flex-1 overflow-hidden rounded bg-muted">
                    <div
                      className="h-full rounded bg-primary"
                      style={{ width: `${Math.max(2, (tokens / trendMax) * 100)}%` }}
                    />
                  </div>
                  <span className="w-24 shrink-0 text-right font-mono">{formatTokens(tokens)}</span>
                  <span className="w-14 shrink-0 text-right text-muted-foreground">
                    {point.entries}
                  </span>
                </div>
              );
            })}
          </div>
        </Card>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <h2 className="mb-3 text-sm font-semibold">{t("usage.byModel")}</h2>
          {!stats || stats.by_model.length === 0 ? (
            <Empty text={t("usage.empty")} />
          ) : (
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">model</th>
                  <th className="text-right">{t("usage.calls")}</th>
                  <th className="text-right">{t("usage.tokens")}</th>
                  <th className="text-right">{t("usage.failed")}</th>
                </tr>
              </thead>
              <tbody>
                {stats.by_model.map((group) => (
                  <tr key={group.name} className="border-t">
                    <td className="py-1.5 font-mono text-xs">{group.name}</td>
                    <td className="text-right">{group.entries}</td>
                    <td className="text-right">
                      {formatTokens(group.prompt_tokens + group.completion_tokens)}
                    </td>
                    <td className="text-right">{group.failed}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>

        <Card>
          <h2 className="mb-3 text-sm font-semibold">{t("usage.byProvider")}</h2>
          {!stats || stats.by_provider.length === 0 ? (
            <Empty text={t("usage.empty")} />
          ) : (
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">provider</th>
                  <th className="text-right">{t("usage.calls")}</th>
                  <th className="text-right">{t("usage.tokens")}</th>
                  <th className="text-right">{t("usage.failed")}</th>
                </tr>
              </thead>
              <tbody>
                {stats.by_provider.map((group) => (
                  <tr key={group.name} className="border-t">
                    <td className="py-1.5 font-mono text-xs">{group.name}</td>
                    <td className="text-right">{group.entries}</td>
                    <td className="text-right">
                      {formatTokens(group.prompt_tokens + group.completion_tokens)}
                    </td>
                    <td className="text-right">{group.failed}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>
      </div>

      <Card>
        <h2 className="mb-3 text-sm font-semibold">{t("usage.recent")}</h2>
        {entries.length === 0 ? (
          <Empty text={t("usage.empty")} />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">{t("usage.time")}</th>
                  <th>model</th>
                  <th>provider</th>
                  <th className="text-right">{t("usage.tokens")}</th>
                  <th className="text-right">ms</th>
                  <th className="text-right">{t("common.status")}</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((entry) => (
                  <tr key={entry.id} className="border-t">
                    <td className="py-1.5 text-xs text-muted-foreground">
                      {formatTime(entry.created_at)}
                    </td>
                    <td className="font-mono text-xs">
                      {entry.model}
                      {entry.stream ? <span className="ml-1 text-muted-foreground">·stream</span> : null}
                    </td>
                    <td className="font-mono text-xs">{entry.provider_id || "—"}</td>
                    <td className="text-right">
                      {formatTokens(entry.prompt_tokens + entry.completion_tokens)}
                    </td>
                    <td className="text-right text-xs">{entry.duration_ms ?? 0}</td>
                    <td className="text-right text-xs">
                      {entry.error_code ? (
                        <span className="text-danger">{entry.error_code}</span>
                      ) : (
                        entry.status || 200
                      )}
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
