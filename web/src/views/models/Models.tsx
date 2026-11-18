import { useCallback, useEffect, useState } from "react";
import { KeyRound, Play, Plus, RefreshCw, Square, Trash2 } from "lucide-react";
import { getMe } from "@/api/account";
import { listModels } from "@/api/insight";
import {
  createCredential,
  createProvider,
  createRoute,
  deleteCredential,
  deleteProvider,
  deleteRoute,
  listCredentials,
  listProviders,
  listRoutes,
  updateProvider,
} from "@/api/providers";
import type {
  Credential,
  Me,
  ModelInfo,
  Provider,
  ProviderKind,
  Route,
} from "@/api/types";
import { useI18n } from "@/i18n";
import { ApiError } from "@/lib/api";
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
  StatusBadge,
} from "@/components/ui";

const KINDS: ProviderKind[] = ["openai", "anthropic", "custom"];

export default function Models() {
  const { t } = useI18n();

  const [me, setMe] = useState<Me | null>(null);
  const [providers, setProviders] = useState<Provider[] | null>(null);
  const [credentials, setCredentials] = useState<Record<string, Credential[]>>({});
  const [routes, setRoutes] = useState<Route[] | null>(null);
  const [exposed, setExposed] = useState<ModelInfo[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [unavailable, setUnavailable] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const [providerForm, setProviderForm] = useState({
    id: "",
    name: "",
    kind: "openai" as ProviderKind,
    base_url: "",
  });
  const [credentialForm, setCredentialForm] = useState({
    provider_id: "",
    label: "",
    secret: "",
  });
  const [routeForm, setRouteForm] = useState({
    exposed_model: "",
    provider_id: "",
    upstream_model: "",
    aliases: "",
    priority: "0",
  });

  const canAdmin = Boolean(me?.scopes.includes("admin"));

  const load = useCallback(async () => {
    setError(null);
    try {
      setMe(await getMe());

      // Exposed names come from the proxy; a key without chat scope cannot ask,
      // which must not break the management view.
      listModels()
        .then((data) => setExposed(data.data))
        .catch(() => setExposed([]));

      let list: Provider[];
      try {
        list = (await listProviders()).providers;
      } catch (err) {
        if (err instanceof ApiError && err.code === "providers_unavailable") {
          setUnavailable(err.message);
          setProviders([]);
          setCredentials({});
          setRoutes([]);
          return;
        }
        throw err;
      }

      setUnavailable(null);
      setProviders(list);

      const [credentialSets, routeSet] = await Promise.all([
        Promise.all(
          list.map(async (provider) => {
            const data = await listCredentials(provider.id);
            return [provider.id, data.credentials] as const;
          })
        ),
        listRoutes(),
      ]);
      setCredentials(Object.fromEntries(credentialSets));
      setRoutes(routeSet.routes);
    } catch (err) {
      setError(err);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Keep the two "belongs to which provider" selects pointing at something real.
  useEffect(() => {
    if (!providers || providers.length === 0) return;
    const first = providers[0].id;
    setCredentialForm((form) => (form.provider_id ? form : { ...form, provider_id: first }));
    setRouteForm((form) => (form.provider_id ? form : { ...form, provider_id: first }));
  }, [providers]);

  async function run(key: string, action: () => Promise<unknown>) {
    setBusy(key);
    setError(null);
    try {
      await action();
      await load();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(null);
    }
  }

  async function onCreateProvider(event: React.FormEvent) {
    event.preventDefault();
    await run("provider:create", async () => {
      await createProvider({
        id: providerForm.id.trim(),
        name: providerForm.name.trim() || undefined,
        kind: providerForm.kind,
        base_url: providerForm.base_url.trim(),
      });
      setProviderForm({ id: "", name: "", kind: "openai", base_url: "" });
    });
  }

  async function onCreateCredential(event: React.FormEvent) {
    event.preventDefault();
    await run("credential:create", async () => {
      await createCredential(credentialForm.provider_id, {
        label: credentialForm.label.trim() || undefined,
        secret: credentialForm.secret,
      });
      setCredentialForm({ ...credentialForm, label: "", secret: "" });
    });
  }

  async function onCreateRoute(event: React.FormEvent) {
    event.preventDefault();
    await run("route:create", async () => {
      await createRoute({
        exposed_model: routeForm.exposed_model.trim(),
        provider_id: routeForm.provider_id,
        upstream_model: routeForm.upstream_model.trim(),
        aliases: routeForm.aliases
          .split(",")
          .map((part) => part.trim())
          .filter(Boolean),
        priority: Number(routeForm.priority) || 0,
      });
      setRouteForm({ ...routeForm, exposed_model: "", upstream_model: "", aliases: "" });
    });
  }

  return (
    <>
      <PageHeader
        title={t("models.title")}
        description={t("models.description")}
        actions={
          <Button
            variant="ghost"
            disabled={busy === "refresh"}
            onClick={() => run("refresh", async () => undefined)}
          >
            <RefreshCw className="h-4 w-4" /> {t("models.refresh")}
          </Button>
        }
      />

      {error ? <ErrorBox error={error} /> : null}

      {me && !canAdmin ? (
        <div className="rounded-lg border border-amber-400/40 bg-amber-400/10 px-4 py-3 text-sm">
          {t("models.readOnly")}
        </div>
      ) : null}

      {unavailable ? (
        <Card>
          <h2 className="mb-2 text-sm font-semibold">{t("models.unavailableTitle")}</h2>
          <p className="text-sm text-muted-foreground">{t("models.unavailableBody")}</p>
          <pre className="mt-3 overflow-auto rounded-lg bg-muted/50 p-3 font-mono text-xs">{`export JANUS_MASTER_KEY=$(openssl rand -hex 32)`}</pre>
        </Card>
      ) : null}

      <Card>
        <h2 className="mb-1 text-sm font-semibold">{t("models.exposed")}</h2>
        <p className="mb-3 text-xs text-muted-foreground">{t("models.exposedHint")}</p>
        {exposed.length === 0 ? (
          <Empty text={t("models.empty")} />
        ) : (
          <div className="flex flex-wrap gap-2">
            {exposed.map((model) => (
              <Badge key={model.id} className="bg-muted font-mono text-xs">
                {model.id}
              </Badge>
            ))}
          </div>
        )}
      </Card>

      <Card>
        <h2 className="mb-1 text-sm font-semibold">{t("models.providerTitle")}</h2>
        <p className="mb-3 text-xs text-muted-foreground">{t("models.providerHint")}</p>

        {providers === null ? (
          <Loading />
        ) : providers.length === 0 ? (
          <Empty text={t("models.providerEmpty")} />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">{t("models.providerId")}</th>
                  <th>{t("models.providerKind")}</th>
                  <th>{t("models.baseUrl")}</th>
                  <th>{t("common.status")}</th>
                  <th className="text-right">{t("common.none")}</th>
                </tr>
              </thead>
              <tbody>
                {providers.map((provider) => (
                  <tr key={provider.id} className="border-t">
                    <td className="py-2">
                      <div className="font-medium">{provider.name || provider.id}</div>
                      <div className="font-mono text-xs text-muted-foreground">{provider.id}</div>
                    </td>
                    <td className="text-xs">{provider.kind}</td>
                    <td className="max-w-[260px] truncate font-mono text-xs text-muted-foreground">
                      {provider.base_url}
                    </td>
                    <td>
                      <Badge
                        className={
                          provider.enabled
                            ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300"
                            : "bg-slate-200 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300"
                        }
                      >
                        {provider.enabled ? t("models.on") : t("models.off")}
                      </Badge>
                    </td>
                    <td className="py-2 text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          disabled={!canAdmin || busy === `provider:${provider.id}`}
                          onClick={() =>
                            run(`provider:${provider.id}`, () =>
                              updateProvider(provider.id, { enabled: !provider.enabled })
                            )
                          }
                        >
                          {provider.enabled ? (
                            <Square className="h-3.5 w-3.5" />
                          ) : (
                            <Play className="h-3.5 w-3.5" />
                          )}
                          {provider.enabled ? t("models.disable") : t("models.enable")}
                        </Button>
                        <Button
                          variant="ghost"
                          disabled={!canAdmin || busy === `provider:${provider.id}`}
                          onClick={() => {
                            if (!window.confirm(t("models.deleteProviderConfirm"))) return;
                            void run(`provider:${provider.id}`, () => deleteProvider(provider.id));
                          }}
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                          {t("models.delete")}
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        {canAdmin ? (
          <form onSubmit={onCreateProvider} className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <Field label={t("models.providerId")}>
              <Input
                required
                placeholder="openai"
                value={providerForm.id}
                onChange={(event) => setProviderForm({ ...providerForm, id: event.target.value })}
              />
            </Field>
            <Field label={t("models.providerName")}>
              <Input
                placeholder="OpenAI"
                value={providerForm.name}
                onChange={(event) => setProviderForm({ ...providerForm, name: event.target.value })}
              />
            </Field>
            <Field label={t("models.providerKind")}>
              <Select
                value={providerForm.kind}
                onChange={(event) =>
                  setProviderForm({ ...providerForm, kind: event.target.value as ProviderKind })
                }
              >
                {KINDS.map((kind) => (
                  <option key={kind} value={kind}>
                    {kind}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t("models.baseUrl")}>
              <Input
                required
                placeholder="https://api.openai.com/v1"
                value={providerForm.base_url}
                onChange={(event) =>
                  setProviderForm({ ...providerForm, base_url: event.target.value })
                }
              />
            </Field>
            <div className="sm:col-span-2 lg:col-span-4">
              <Button type="submit" disabled={busy === "provider:create"}>
                <Plus className="h-4 w-4" />
                {busy === "provider:create" ? t("models.saving") : t("models.addProvider")}
              </Button>
            </div>
          </form>
        ) : null}
      </Card>

      <Card>
        <h2 className="mb-1 text-sm font-semibold">{t("models.credentials")}</h2>
        <p className="mb-3 text-xs text-muted-foreground">{t("models.credentialsHint")}</p>

        {providers && providers.length > 0 ? (
          <>
            <div className="mb-3 flex flex-wrap items-end gap-3">
              <Field label={t("models.credentialFor")} className="w-56">
                <Select
                  value={credentialForm.provider_id}
                  onChange={(event) =>
                    setCredentialForm({ ...credentialForm, provider_id: event.target.value })
                  }
                >
                  {providers.map((provider) => (
                    <option key={provider.id} value={provider.id}>
                      {provider.name || provider.id}
                    </option>
                  ))}
                </Select>
              </Field>
            </div>

            {(credentials[credentialForm.provider_id] || []).length === 0 ? (
              <Empty text={t("models.credentialEmpty")} />
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead className="text-left text-xs text-muted-foreground">
                    <tr>
                      <th className="py-2">{t("models.credentialLabel")}</th>
                      <th>{t("models.masked")}</th>
                      <th>{t("common.status")}</th>
                      <th className="text-right">{t("common.none")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(credentials[credentialForm.provider_id] || []).map((credential) => (
                      <tr key={credential.id} className="border-t">
                        <td className="py-2">
                          <div>{credential.label || credential.id}</div>
                          <div className="font-mono text-xs text-muted-foreground">
                            {credential.kind}
                          </div>
                        </td>
                        <td className="font-mono text-xs">{credential.masked || "—"}</td>
                        <td>
                          <StatusBadge status={credential.status} />
                        </td>
                        <td className="py-2 text-right">
                          <Button
                            variant="ghost"
                            disabled={!canAdmin || busy === `credential:${credential.id}`}
                            onClick={() => {
                              if (!window.confirm(t("models.deleteCredentialConfirm"))) return;
                              void run(`credential:${credential.id}`, () =>
                                deleteCredential(credential.id)
                              );
                            }}
                          >
                            <Trash2 className="h-3.5 w-3.5" />
                            {t("models.delete")}
                          </Button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            {canAdmin ? (
              <form onSubmit={onCreateCredential} className="mt-4 grid gap-3 sm:grid-cols-2">
                <Field label={t("models.credentialLabel")}>
                  <Input
                    placeholder={t("models.credentialLabelPlaceholder")}
                    value={credentialForm.label}
                    onChange={(event) =>
                      setCredentialForm({ ...credentialForm, label: event.target.value })
                    }
                  />
                </Field>
                <Field label={t("models.secret")}>
                  <Input
                    required
                    type="password"
                    autoComplete="off"
                    placeholder={t("models.secretPlaceholder")}
                    value={credentialForm.secret}
                    onChange={(event) =>
                      setCredentialForm({ ...credentialForm, secret: event.target.value })
                    }
                  />
                </Field>
                <div className="sm:col-span-2">
                  <Button type="submit" disabled={busy === "credential:create"}>
                    <KeyRound className="h-4 w-4" />
                    {busy === "credential:create"
                      ? t("models.saving")
                      : t("models.addCredential")}
                  </Button>
                </div>
              </form>
            ) : null}
          </>
        ) : (
          <Empty text={t("models.providerEmpty")} />
        )}
      </Card>

      <Card>
        <h2 className="mb-1 text-sm font-semibold">{t("models.routes")}</h2>
        <p className="mb-3 text-xs text-muted-foreground">{t("models.routesHint")}</p>

        {routes === null ? (
          <Loading />
        ) : routes.length === 0 ? (
          <Empty text={t("models.routeEmpty")} />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-muted-foreground">
                <tr>
                  <th className="py-2">{t("models.exposedModel")}</th>
                  <th>{t("models.providerTitle")}</th>
                  <th>{t("models.upstreamModel")}</th>
                  <th>{t("models.priority")}</th>
                  <th className="text-right">{t("common.none")}</th>
                </tr>
              </thead>
              <tbody>
                {routes.map((route) => (
                  <tr key={route.id} className="border-t">
                    <td className="py-2">
                      <div className="font-mono text-xs">{route.exposed_model}</div>
                      {(route.aliases || []).length > 0 ? (
                        <div className="text-xs text-muted-foreground">
                          {route.aliases?.join(", ")}
                        </div>
                      ) : null}
                    </td>
                    <td className="font-mono text-xs">{route.provider_id}</td>
                    <td className="font-mono text-xs">{route.upstream_model}</td>
                    <td>{route.priority}</td>
                    <td className="py-2 text-right">
                      <Button
                        variant="ghost"
                        disabled={!canAdmin || busy === `route:${route.id}`}
                        onClick={() => {
                          if (!window.confirm(t("models.deleteRouteConfirm"))) return;
                          void run(`route:${route.id}`, () => deleteRoute(route.id));
                        }}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                        {t("models.delete")}
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        {canAdmin && providers && providers.length > 0 ? (
          <form onSubmit={onCreateRoute} className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <Field label={t("models.exposedModel")}>
              <Input
                required
                placeholder="fast"
                value={routeForm.exposed_model}
                onChange={(event) =>
                  setRouteForm({ ...routeForm, exposed_model: event.target.value })
                }
              />
            </Field>
            <Field label={t("models.providerTitle")}>
              <Select
                value={routeForm.provider_id}
                onChange={(event) =>
                  setRouteForm({ ...routeForm, provider_id: event.target.value })
                }
              >
                {providers.map((provider) => (
                  <option key={provider.id} value={provider.id}>
                    {provider.name || provider.id}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t("models.upstreamModel")}>
              <Input
                required
                placeholder="gpt-4o-mini"
                value={routeForm.upstream_model}
                onChange={(event) =>
                  setRouteForm({ ...routeForm, upstream_model: event.target.value })
                }
              />
            </Field>
            <Field label={t("models.aliases")}>
              <Input
                placeholder={t("models.aliasesHint")}
                value={routeForm.aliases}
                onChange={(event) => setRouteForm({ ...routeForm, aliases: event.target.value })}
              />
            </Field>
            <Field label={t("models.priority")}>
              <Input
                type="number"
                min={0}
                value={routeForm.priority}
                onChange={(event) => setRouteForm({ ...routeForm, priority: event.target.value })}
              />
            </Field>
            <div className="sm:col-span-2 lg:col-span-3">
              <Button type="submit" disabled={busy === "route:create"}>
                <Plus className="h-4 w-4" />
                {busy === "route:create" ? t("models.saving") : t("models.addRoute")}
              </Button>
            </div>
          </form>
        ) : null}
      </Card>
    </>
  );
}
