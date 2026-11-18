import { useCallback, useEffect, useState } from "react";
import {
  Activity,
  ArrowDown,
  ArrowUp,
  Download,
  KeyRound,
  Play,
  Plus,
  RefreshCw,
  RotateCcw,
  Square,
  Trash2,
} from "lucide-react";
import { getMe } from "@/api/account";
import { listModels } from "@/api/insight";
import {
  completeOAuth,
  createCredential,
  createProvider,
  createRoute,
  deleteCredential,
  deleteProvider,
  deleteRoute,
  importLocal,
  listChannels,
  listCredentials,
  listProviders,
  listRoutes,
  pollDevice,
  probeProvider,
  refreshQuota,
  resetCredential,
  startDevice,
  startOAuth,
  syncRoutes,
  updateCredential,
  updateProvider,
} from "@/api/providers";
import type {
  Channel,
  Credential,
  Me,
  ModelInfo,
  OAuthConfig,
  ProbeResult,
  Provider,
  ProviderKind,
  Quota,
  Route,
  SyncResult,
} from "@/api/types";
import type { ImportSource } from "@/api/providers";
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

function ProbeCell({ result }: { result?: ProbeResult }) {
  const { t } = useI18n();
  if (!result) {
    return <span className="text-xs text-muted-foreground">{t("common.none")}</span>;
  }
  if (result.ok) {
    return (
      <Badge className="bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300">
        {t("models.probeOk", { latency: result.latency_ms, count: result.model_count })}
      </Badge>
    );
  }
  return (
    <span className="text-xs text-danger" title={result.error}>
      {t("models.probeFailed")}
    </span>
  );
}

function QuotaCell({ quota }: { quota?: Quota }) {
  const { t } = useI18n();
  if (!quota) {
    return <span className="text-xs text-muted-foreground">{t("models.quotaUnknown")}</span>;
  }
  if (quota.error) {
    return (
      <span className="text-xs text-danger" title={quota.error}>
        {t("models.quotaFailed")}
      </span>
    );
  }
  const parts: string[] = [];
  if (quota.balance) {
    parts.push(`${quota.balance} ${quota.currency ?? ""}`.trim());
  }
  for (const window of quota.windows ?? []) {
    if (window.used_percent !== undefined) {
      parts.push(`${window.name} ${window.used_percent.toFixed(0)}%`);
    } else if (window.text) {
      parts.push(`${window.name} ${window.text}`);
    }
  }
  if (quota.plan) parts.unshift(quota.plan);
  return (
    <span className="text-xs" title={quota.source}>
      {parts.join(" · ") || t("models.quotaEmpty")}
    </span>
  );
}

export default function Models() {
  const { t } = useI18n();

  const [me, setMe] = useState<Me | null>(null);
  const [providers, setProviders] = useState<Provider[] | null>(null);
  const [credentials, setCredentials] = useState<Record<string, Credential[]>>({});
  const [routes, setRoutes] = useState<Route[] | null>(null);
  const [exposed, setExposed] = useState<ModelInfo[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [unavailable, setUnavailable] = useState<string | null>(null);
  const [probes, setProbes] = useState<Record<string, ProbeResult>>({});
  const [channels, setChannels] = useState<Channel[]>([]);
  const [syncs, setSyncs] = useState<Record<string, SyncResult>>({});
  const [oauth, setOauth] = useState<{
    providerID: string;
    sessionID: string;
    authURL: string;
    credentialID?: string;
  } | null>(null);
  const [oauthCode, setOauthCode] = useState("");
  const [oauthLabel, setOauthLabel] = useState("");
  const [device, setDevice] = useState<{
    providerID: string;
    sessionID: string;
    userCode: string;
    verificationURI: string;
    interval: number;
    credentialID?: string;
  } | null>(null);
  const [importForm, setImportForm] = useState({
    source: "codex" as ImportSource,
    entry: "",
    label: "",
  });
  const [busy, setBusy] = useState<string | null>(null);

  const [providerForm, setProviderForm] = useState({
    id: "",
    name: "",
    kind: "openai" as ProviderKind,
    base_url: "",
    channel: "",
    protocol: "" as "" | "chat" | "responses" | "anthropic",
    oauthJson: "",
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
  const [quick, setQuick] = useState({
    channel: "",
    base_url: "",
    label: "",
    secret: "",
  });
  const [oauthConfigDraft, setOauthConfigDraft] = useState("");

  const canAdmin = Boolean(me?.scopes.includes("admin"));
  const credentialProvider = providers?.find((p) => p.id === credentialForm.provider_id);
  // The backend derives these from the channel: an API-key-only upstream must
  // not offer an OAuth button, and a device-code channel must not be sent to an
  // authorization-code page.
  const authMethods = credentialProvider?.auth ?? [];
  const supportsOAuth = Boolean(credentialProvider?.oauth_ready);
  const supportsDevice = Boolean(credentialProvider?.device_ready);
  const needsOAuthConfig = authMethods.includes("oauth") && !supportsOAuth && !supportsDevice;

  // Quick connect: the channel decides what the next action is, and the
  // upstream is created on demand so signing in is one step.
  const quickChannel = channels.find((channel) => channel.id === quick.channel);
  const quickMethods = quickChannel?.auth ?? ["api_key"];
  const quickOAuth = quickChannel?.login === "oauth";
  // "poll" vendors open a page and are approved out of band, exactly like a
  // device code; the same panel and endpoints serve both.
  const quickDevice =
    quickChannel?.login === "device_code" || quickChannel?.login === "poll";
  const quickNeedsConfig =
    Boolean(quickChannel) &&
    quickMethods.includes("oauth") &&
    !quickOAuth &&
    !quickDevice;
  const quickApiKey = quickMethods.length === 0 || quickMethods.includes("api_key");
  // Local endpoints need no credential at all: creating the upstream is the
  // whole setup.
  const quickNoAuth = quickMethods.length > 0 && quickMethods.every((method) => method === "none");

  const load = useCallback(async () => {
    setError(null);
    try {
      setMe(await getMe());

      // The catalogue is static; a failure only costs the prefill helper.
      listChannels()
        .then((data) => setChannels(data.channels))
        .catch(() => setChannels([]));

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

  // A device sign-in is approved out of band, so the page polls until the
  // backend reports the credential instead of asking for a paste.
  useEffect(() => {
    if (!device) return;
    let cancelled = false;
    const tick = async () => {
      try {
        const result = await pollDevice(device.providerID, device.sessionID);
        if (cancelled) return;
        if (result.status === "complete") {
          setDevice(null);
          await load();
        }
      } catch (err) {
        if (cancelled) return;
        setDevice(null);
        setError(err);
      }
    };
    const timer = window.setInterval(tick, Math.max(2, device.interval) * 1000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [device, load]);

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
    let oauth: OAuthConfig | undefined;
    if (providerForm.oauthJson.trim()) {
      try {
        oauth = JSON.parse(providerForm.oauthJson);
      } catch {
        setError(new Error(t("models.oauthJsonInvalid")));
        return;
      }
    }
    await run("provider:create", async () => {
      await createProvider({
        id: providerForm.id.trim(),
        name: providerForm.name.trim() || undefined,
        kind: providerForm.kind,
        base_url: providerForm.base_url.trim(),
        channel: providerForm.channel || undefined,
        protocol: providerForm.protocol || undefined,
        oauth,
      });
      setProviderForm({
        id: "",
        name: "",
        kind: "openai",
        base_url: "",
        channel: "",
        protocol: "",
        oauthJson: "",
      });
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

  async function onStartOAuth(credentialID?: string, providerID?: string) {
    // Use the id, not a providers lookup: quick connect just created the
    // provider and this render's list may not contain it yet.
    const targetID = providerID ?? credentialForm.provider_id;
    if (!targetID) return;
    setBusy("oauth:start");
    setError(null);
    try {
      const start = await startOAuth(targetID, credentialID);
      setOauth({
        providerID: targetID,
        sessionID: start.session_id,
        authURL: start.auth_url,
        credentialID,
      });
      setOauthCode("");
      setOauthLabel("");
      window.open(start.auth_url, "_blank", "noopener,noreferrer");
    } catch (err) {
      setError(err);
    } finally {
      setBusy(null);
    }
  }

  async function onStartDevice(providerID?: string, label?: string, credentialID?: string) {
    const target = providerID ?? credentialForm.provider_id;
    if (!target) return;
    setBusy("device:start");
    setError(null);
    try {
      const start = await startDevice(target, label, credentialID);
      setDevice({
        providerID: target,
        sessionID: start.session_id,
        userCode: start.user_code,
        verificationURI: start.verification_uri,
        interval: start.interval,
        credentialID,
      });
      window.open(start.verification_uri, "_blank", "noopener,noreferrer");
    } catch (err) {
      setError(err);
    } finally {
      setBusy(null);
    }
  }

  async function onCompleteOAuth() {
    if (!oauth) return;
    setBusy("oauth:complete");
    setError(null);
    try {
      await completeOAuth(
        oauth.providerID,
        oauth.sessionID,
        oauthCode.trim(),
        oauthLabel.trim() || undefined
      );
      setOauth(null);
      setOauthCode("");
      setOauthLabel("");
      await load();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(null);
    }
  }

  async function onImport() {
    if (!credentialForm.provider_id) return;
    await run("import", () =>
      importLocal(credentialForm.provider_id, {
        source: importForm.source,
        entry: importForm.entry.trim() || undefined,
        label: importForm.label.trim() || undefined,
      })
    );
  }

  // moveCredential swaps priorities with the neighbour so one click is one
  // stable position, instead of leaving ties for the pool to break.
  async function moveCredential(list: Credential[], index: number, delta: number) {
    const other = list[index + delta];
    const current = list[index];
    if (!other || !current) return;
    await run(`credential:${current.id}`, async () => {
      await updateCredential(current.id, { priority: other.priority });
      await updateCredential(other.id, { priority: current.priority });
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

  // ensureChannelProvider adopts an existing upstream for the channel or creates
  // one, so the console never asks for "add upstream" as a separate step.
  async function ensureChannelProvider(channel: Channel): Promise<string> {
    const existing = (providers ?? []).find((provider) => provider.channel === channel.id);
    if (existing) return existing.id;
    const base = (quick.base_url.trim() || channel.base_url).trim();
    if (!base) throw new Error(t("models.baseUrlRequired"));
    const taken = new Set((providers ?? []).map((provider) => provider.id));
    let id = channel.id;
    for (let suffix = 2; taken.has(id); suffix += 1) {
      id = `${channel.id}-${suffix}`;
    }
    const created = await createProvider({
      id,
      name: channel.name,
      kind: channel.kind,
      base_url: base,
      channel: channel.id,
    });
    return created.id;
  }

  // A channel without a built-in client needs its PKCE endpoints before a login
  // button can exist; saving them here is what turns oauth_ready on.
  async function onSaveOAuthConfig() {
    if (!credentialForm.provider_id) return;
    let parsed: OAuthConfig;
    try {
      parsed = JSON.parse(oauthConfigDraft) as OAuthConfig;
    } catch {
      setError(new Error(t("models.oauthJsonInvalid")));
      return;
    }
    await run("oauth:config", () =>
      updateProvider(credentialForm.provider_id, { oauth: parsed })
    );
    setOauthConfigDraft("");
  }

  // Used when the channel has nothing to sign in with yet: create the upstream
  // and jump straight to its row so the operator can finish the setup.
  async function onQuickSetup() {
    if (!quickChannel) return;
    setBusy("quick:setup");
    setError(null);
    try {
      const providerID = await ensureChannelProvider(quickChannel);
      setCredentialForm((form) => ({ ...form, provider_id: providerID }));
      await load();
    } catch (err) {
      setError(err);
    } finally {
      setBusy(null);
    }
  }

  async function onQuickConnect(mode: "key" | "oauth" | "device") {
    if (!quickChannel) return;
    setBusy(`quick:${mode}`);
    setError(null);
    try {
      const providerID = await ensureChannelProvider(quickChannel);
      setCredentialForm((form) => ({ ...form, provider_id: providerID }));
      if (mode === "key") {
        if (!quick.secret.trim()) throw new Error(t("models.secretRequired"));
        await createCredential(providerID, {
          label: quick.label.trim() || undefined,
          secret: quick.secret,
        });
        setQuick({ ...quick, secret: "" });
        await load();
        return;
      }
      await load();
      if (mode === "oauth") {
        await onStartOAuth(undefined, providerID);
      } else {
        await onStartDevice(providerID, quick.label.trim() || undefined);
      }
    } catch (err) {
      setError(err);
    } finally {
      setBusy(null);
    }
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
        <h2 className="mb-1 text-sm font-semibold">{t("models.quickTitle")}</h2>
        <p className="mb-3 text-xs text-muted-foreground">{t("models.quickHint")}</p>
        {canAdmin ? (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <Field label={t("models.channel")}>
              <Select
                value={quick.channel}
                onChange={(event) => {
                  const id = event.target.value;
                  const channel = channels.find((item) => item.id === id);
                  setQuick({
                    ...quick,
                    channel: id,
                    base_url: channel?.base_url ?? "",
                  });
                }}
              >
                <option value="">{t("models.quickPick")}</option>
                {channels.map((channel) => (
                  <option key={channel.id} value={channel.id}>
                    {channel.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t("models.baseUrl")}>
              <Input
                value={quick.base_url}
                placeholder="https://api.openai.com/v1"
                onChange={(event) => setQuick({ ...quick, base_url: event.target.value })}
              />
            </Field>
            <Field label={t("models.accountLabel")}>
              <Input
                value={quick.label}
                placeholder={t("models.accountLabelPlaceholder")}
                onChange={(event) => setQuick({ ...quick, label: event.target.value })}
              />
            </Field>
            {quickApiKey ? (
              <Field label={t("models.apiKey")}>
                <Input
                  type="password"
                  autoComplete="off"
                  placeholder={t("models.secretPlaceholder")}
                  value={quick.secret}
                  onChange={(event) => setQuick({ ...quick, secret: event.target.value })}
                />
              </Field>
            ) : null}
            <div className="flex flex-wrap items-end gap-2 sm:col-span-2 lg:col-span-4">
              {quickApiKey ? (
                <Button
                  disabled={!quickChannel || busy === "quick:key"}
                  onClick={() => void onQuickConnect("key")}
                >
                  <Plus className="h-4 w-4" />
                  {busy === "quick:key" ? t("models.saving") : t("models.quickAddKey")}
                </Button>
              ) : null}
              {quickOAuth ? (
                <Button
                  disabled={!quickChannel || busy === "quick:oauth"}
                  onClick={() => void onQuickConnect("oauth")}
                >
                  <KeyRound className="h-4 w-4" />
                  {busy === "quick:oauth" ? t("models.oauthStarting") : t("models.oauthLogin")}
                </Button>
              ) : null}
              {quickDevice ? (
                <Button
                  disabled={!quickChannel || busy === "quick:device"}
                  onClick={() => void onQuickConnect("device")}
                >
                  <KeyRound className="h-4 w-4" />
                  {busy === "quick:device" ? t("models.deviceStarting") : t("models.deviceLogin")}
                </Button>
              ) : null}
              {quickNeedsConfig || quickNoAuth ? (
                <Button
                  disabled={!quickChannel || busy === "quick:setup"}
                  onClick={() => void onQuickSetup()}
                >
                  <Plus className="h-4 w-4" />
                  {busy === "quick:setup"
                    ? t("models.saving")
                    : quickNoAuth
                      ? t("models.quickConnect")
                      : t("models.quickSetup")}
                </Button>
              ) : null}
              {quickNeedsConfig ? (
                <p className="basis-full text-xs text-amber-600 dark:text-amber-400">
                  {t("models.oauthNeedsConfig")}
                </p>
              ) : null}
              {quickChannel?.note ? (
                <p className="basis-full text-xs text-muted-foreground">{quickChannel.note}</p>
              ) : null}
            </div>
          </div>
        ) : (
          <p className="text-xs text-muted-foreground">{t("models.readOnly")}</p>
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
                  <th>{t("models.probe")}</th>
                  <th className="text-right">{t("common.none")}</th>
                </tr>
              </thead>
              <tbody>
                {providers.map((provider) => (
                  <tr key={provider.id} className="border-t">
                    <td className="py-2">
                      <div className="font-medium">{provider.name || provider.id}</div>
                      <div className="font-mono text-xs text-muted-foreground">{provider.id}</div>
                      {syncs[provider.id] ? (
                        <div className="text-xs text-muted-foreground">
                          {syncs[provider.id].error
                            ? t("models.syncFailed")
                            : t("models.syncSummary", {
                                created: syncs[provider.id].created.length,
                                existing: syncs[provider.id].existing.length,
                                conflicts: syncs[provider.id].conflicts.length,
                                stale: syncs[provider.id].stale.length,
                              })}
                        </div>
                      ) : null}
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
                    <td>
                      <ProbeCell result={probes[provider.id]} />
                    </td>
                    <td className="py-2 text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          disabled={!canAdmin || busy === `probe:${provider.id}`}
                          onClick={() =>
                            run(`probe:${provider.id}`, async () => {
                              const result = await probeProvider(provider.id);
                              setProbes((current) => ({ ...current, [provider.id]: result }));
                            })
                          }
                        >
                          <Activity className="h-3.5 w-3.5" />
                          {busy === `probe:${provider.id}` ? t("models.probing") : t("models.probe")}
                        </Button>
                        <Button
                          variant="ghost"
                          title={t("models.syncRoutesHint")}
                          disabled={!canAdmin || busy === `sync:${provider.id}`}
                          onClick={() =>
                            run(`sync:${provider.id}`, async () => {
                              const result = await syncRoutes(provider.id);
                              setSyncs((current) => ({ ...current, [provider.id]: result }));
                            })
                          }
                        >
                          <RefreshCw className="h-3.5 w-3.5" />
                          {busy === `sync:${provider.id}`
                            ? t("models.syncing")
                            : t("models.syncRoutes")}
                        </Button>
                        {syncs[provider.id]?.stale.length ? (
                          <Button
                            variant="ghost"
                            disabled={!canAdmin || busy === `sync:${provider.id}`}
                            onClick={() =>
                              run(`sync:${provider.id}`, async () => {
                                const result = await syncRoutes(provider.id, true);
                                setSyncs((current) => ({ ...current, [provider.id]: result }));
                              })
                            }
                          >
                            <Trash2 className="h-3.5 w-3.5" />
                            {t("models.pruneStale")}
                          </Button>
                        ) : null}
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
            <Field label={t("models.channel")}>
              <Select
                value={providerForm.channel}
                onChange={(event) => {
                  const id = event.target.value;
                  const channel = channels.find((item) => item.id === id);
                  if (!channel) {
                    setProviderForm({ ...providerForm, channel: "" });
                    return;
                  }
                  setProviderForm({
                    id: providerForm.id || channel.id,
                    name: providerForm.name || channel.name,
                    kind: channel.kind,
                    base_url: channel.base_url || providerForm.base_url,
                    channel: channel.id,
                    protocol: "",
                    oauthJson: providerForm.oauthJson,
                  });
                }}
              >
                <option value="">{t("models.channelCustom")}</option>
                {channels.map((channel) => (
                  <option key={channel.id} value={channel.id}>
                    {channel.name}
                  </option>
                ))}
              </Select>
            </Field>
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
            <Field label={t("models.protocol")}>
              <Select
                value={providerForm.protocol}
                onChange={(event) =>
                  setProviderForm({
                    ...providerForm,
                    protocol: event.target.value as "" | "chat" | "responses" | "anthropic",
                  })
                }
              >
                <option value="">{t("models.protocolAuto")}</option>
                <option value="chat">chat</option>
                <option value="responses">responses</option>
                <option value="anthropic">anthropic</option>
              </Select>
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
            <Field label={t("models.oauthConfig")} className="sm:col-span-2 lg:col-span-4">
              <textarea
                rows={3}
                spellCheck={false}
                placeholder='{"client_id":"...","authorize_url":"https://...","token_url":"https://...","redirect_uri":"https://..."}'
                className="w-full rounded-lg border bg-transparent px-3 py-2 font-mono text-xs outline-hidden focus-visible:ring-2 focus-visible:ring-ring"
                value={providerForm.oauthJson}
                onChange={(event) =>
                  setProviderForm({ ...providerForm, oauthJson: event.target.value })
                }
              />
              <p className="mt-1 text-xs text-muted-foreground">{t("models.oauthConfigHint")}</p>
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
              <Button
                variant="ghost"
                disabled={!canAdmin || busy === `quota:${credentialForm.provider_id}`}
                onClick={() =>
                  run(`quota:${credentialForm.provider_id}`, () =>
                    refreshQuota(credentialForm.provider_id)
                  )
                }
              >
                <RefreshCw className="h-3.5 w-3.5" />
                {busy === `quota:${credentialForm.provider_id}`
                  ? t("models.quotaRefreshing")
                  : t("models.quotaRefresh")}
              </Button>
            </div>

            {canAdmin && needsOAuthConfig ? (
              <div className="mb-3 space-y-2 rounded-lg border border-amber-400/40 bg-amber-400/10 px-4 py-3">
                <p className="text-xs">{t("models.oauthNeedsConfig")}</p>
                <textarea
                  rows={3}
                  spellCheck={false}
                  placeholder='{"client_id":"...","authorize_url":"https://...","token_url":"https://...","redirect_uri":"https://..."}'
                  className="w-full rounded-lg border bg-transparent px-3 py-2 font-mono text-xs outline-hidden focus-visible:ring-2 focus-visible:ring-ring"
                  value={oauthConfigDraft}
                  onChange={(event) => setOauthConfigDraft(event.target.value)}
                />
                <Button
                  disabled={!oauthConfigDraft.trim() || busy === "oauth:config"}
                  onClick={() => void onSaveOAuthConfig()}
                >
                  {busy === "oauth:config" ? t("models.saving") : t("models.oauthSaveConfig")}
                </Button>
              </div>
            ) : null}

            {canAdmin && supportsOAuth ? (
              <div className="mb-3 rounded-lg border p-3">
                {!oauth || oauth.providerID !== credentialForm.provider_id ? (
                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <p className="text-xs text-muted-foreground">
                      {credentialProvider?.oauth
                        ? t("models.oauthCustomHint")
                        : t("models.oauthHint")}
                    </p>
                    <Button
                      variant="ghost"
                      disabled={busy === "oauth:start"}
                      onClick={() => onStartOAuth()}
                    >
                      <KeyRound className="h-4 w-4" />
                      {busy === "oauth:start" ? t("models.oauthStarting") : t("models.oauthLogin")}
                    </Button>
                  </div>
                ) : (
                  <div className="space-y-2">
                    <p className="text-xs text-muted-foreground">
                      {oauth.credentialID
                        ? t("models.oauthReauthHint")
                        : t("models.oauthPasteHint")}
                    </p>
                    <div className="flex flex-wrap items-end gap-2">
                      <Field label={t("models.oauthCode")} className="min-w-[280px] flex-1">
                        <Input
                          value={oauthCode}
                          placeholder={t("models.oauthCodePlaceholder")}
                          onChange={(event) => setOauthCode(event.target.value)}
                        />
                      </Field>
                      <Field label={t("models.accountLabel")} className="w-48">
                        <Input
                          value={oauthLabel}
                          placeholder={t("models.accountLabelPlaceholder")}
                          onChange={(event) => setOauthLabel(event.target.value)}
                        />
                      </Field>
                      <Button
                        disabled={!oauthCode.trim() || busy === "oauth:complete"}
                        onClick={onCompleteOAuth}
                      >
                        {busy === "oauth:complete"
                          ? t("models.saving")
                          : t("models.oauthComplete")}
                      </Button>
                      <Button
                        variant="ghost"
                        onClick={() => {
                          setOauth(null);
                          setOauthCode("");
                          setOauthLabel("");
                        }}
                      >
                        {t("models.oauthCancel")}
                      </Button>
                    </div>
                    <p className="text-xs text-muted-foreground">
                      <a
                        href={oauth.authURL}
                        target="_blank"
                        rel="noreferrer"
                        className="underline"
                      >
                        {t("models.oauthReopen")}
                      </a>
                    </p>
                  </div>
                )}
              </div>
            ) : null}

            {canAdmin && supportsDevice ? (
              <div className="mb-3 rounded-lg border p-3">
                {!device || device.providerID !== credentialForm.provider_id ? (
                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <p className="text-xs text-muted-foreground">{t("models.deviceHint")}</p>
                    <Button
                      variant="ghost"
                      disabled={busy === "device:start"}
                      onClick={() => void onStartDevice()}
                    >
                      <KeyRound className="h-4 w-4" />
                      {busy === "device:start" ? t("models.deviceStarting") : t("models.deviceLogin")}
                    </Button>
                  </div>
                ) : (
                  <div className="space-y-2">
                    <p className="text-xs text-muted-foreground">
                      {device.credentialID ? t("models.deviceReauthHint") : t("models.deviceCodeHint")}
                    </p>
                    <div className="flex flex-wrap items-center gap-3">
                      {device.userCode ? (
                        <code className="rounded bg-muted px-3 py-1.5 font-mono text-lg tracking-widest">
                          {device.userCode}
                        </code>
                      ) : null}
                      <a
                        href={device.verificationURI}
                        target="_blank"
                        rel="noreferrer"
                        className="text-xs underline"
                      >
                        {t("models.deviceOpen")}
                      </a>
                      <span className="text-xs text-muted-foreground">{t("models.deviceWaiting")}</span>
                      <Button variant="ghost" onClick={() => setDevice(null)}>
                        {t("models.oauthCancel")}
                      </Button>
                    </div>
                  </div>
                )}
              </div>
            ) : null}

            {canAdmin ? (
              <div className="mb-3 flex flex-wrap items-end gap-2 rounded-lg border border-dashed p-3">
                <Field label={t("models.importSource")} className="w-40">
                  <Select
                    value={importForm.source}
                    onChange={(event) =>
                      setImportForm({ ...importForm, source: event.target.value as ImportSource })
                    }
                  >
                    <option value="codex">Codex</option>
                    <option value="claude">Claude Code</option>
                    <option value="opencode">OpenCode</option>
                  </Select>
                </Field>
                <Field label={t("models.importEntry")} className="w-44">
                  <Input
                    value={importForm.entry}
                    placeholder={t("models.importEntryPlaceholder")}
                    onChange={(event) =>
                      setImportForm({ ...importForm, entry: event.target.value })
                    }
                  />
                </Field>
                <Button variant="ghost" disabled={busy === "import"} onClick={onImport}>
                  <Download className="h-4 w-4" />
                  {busy === "import" ? t("models.importing") : t("models.importLocal")}
                </Button>
                <p className="basis-full text-xs text-muted-foreground">
                  {t("models.importHint")}
                </p>
              </div>
            ) : null}

            {(credentials[credentialForm.provider_id] || []).length === 0 ? (
              <Empty text={t("models.credentialEmpty")} />
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead className="text-left text-xs text-muted-foreground">
                    <tr>
                      <th className="py-2">{t("models.account")}</th>
                      <th>{t("models.masked")}</th>
                      <th>{t("common.status")}</th>
                      <th>{t("models.quota")}</th>
                      <th>{t("models.priority")}</th>
                      <th className="text-right">{t("common.none")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(credentials[credentialForm.provider_id] || []).map(
                      (credential, index, list) => (
                        <tr key={credential.id} className="border-t align-top">
                          <td className="py-2">
                            <div className="font-medium">
                              {credential.label || credential.account || credential.id}
                            </div>
                            <div className="text-xs text-muted-foreground">
                              {credential.account && credential.account !== credential.label
                                ? credential.account
                                : null}
                              {credential.plan ? ` · ${credential.plan}` : ""}
                              {credential.kind === "oauth" ? " · oauth" : ""}
                              {credential.disabled ? ` · ${t("models.disabled")}` : ""}
                            </div>
                          </td>
                          <td className="font-mono text-xs">{credential.masked || "—"}</td>
                          <td>
                            <StatusBadge status={credential.status} />
                            {credential.last_error ? (
                              <div
                                className="max-w-[180px] truncate text-xs text-muted-foreground"
                                title={credential.last_error}
                              >
                                {credential.last_error}
                              </div>
                            ) : null}
                          </td>
                          <td>
                            <QuotaCell quota={credential.quota} />
                          </td>
                          <td className="font-mono text-xs">{credential.priority}</td>
                          <td className="py-2 text-right">
                            <div className="flex flex-wrap justify-end gap-1">
                              <Button
                                variant="ghost"
                                title={t("models.moveUp")}
                                disabled={!canAdmin || index === 0 || busy === `credential:${credential.id}`}
                                onClick={() => void moveCredential(list, index, -1)}
                              >
                                <ArrowUp className="h-3.5 w-3.5" />
                              </Button>
                              <Button
                                variant="ghost"
                                title={t("models.moveDown")}
                                disabled={
                                  !canAdmin ||
                                  index === list.length - 1 ||
                                  busy === `credential:${credential.id}`
                                }
                                onClick={() => void moveCredential(list, index, 1)}
                              >
                                <ArrowDown className="h-3.5 w-3.5" />
                              </Button>
                              {canAdmin &&
                              credential.kind === "oauth" &&
                              (supportsOAuth || supportsDevice) ? (
                                <Button
                                  variant="ghost"
                                  title={t("models.reauthorize")}
                                  disabled={busy === `credential:${credential.id}`}
                                  onClick={() => {
                                    setCredentialForm({
                                      ...credentialForm,
                                      provider_id: credential.provider_id,
                                    });
                                    if (supportsDevice) {
                                      void onStartDevice(
                                        credential.provider_id,
                                        undefined,
                                        credential.id
                                      );
                                    } else {
                                      void onStartOAuth(credential.id);
                                    }
                                  }}
                                >
                                  <KeyRound className="h-3.5 w-3.5" />
                                </Button>
                              ) : null}
                              <Button
                                variant="ghost"
                                title={t("models.reset")}
                                disabled={!canAdmin || busy === `credential:${credential.id}`}
                                onClick={() =>
                                  void run(`credential:${credential.id}`, () =>
                                    resetCredential(credential.id)
                                  )
                                }
                              >
                                <RotateCcw className="h-3.5 w-3.5" />
                              </Button>
                              <Button
                                variant="ghost"
                                title={credential.disabled ? t("models.enable") : t("models.disable")}
                                disabled={!canAdmin || busy === `credential:${credential.id}`}
                                onClick={() =>
                                  void run(`credential:${credential.id}`, () =>
                                    updateCredential(credential.id, {
                                      disabled: !credential.disabled,
                                    })
                                  )
                                }
                              >
                                {credential.disabled ? (
                                  <Play className="h-3.5 w-3.5" />
                                ) : (
                                  <Square className="h-3.5 w-3.5" />
                                )}
                              </Button>
                              <Button
                                variant="ghost"
                                title={t("models.delete")}
                                disabled={!canAdmin || busy === `credential:${credential.id}`}
                                onClick={() => {
                                  if (!window.confirm(t("models.deleteCredentialConfirm"))) return;
                                  void run(`credential:${credential.id}`, () =>
                                    deleteCredential(credential.id)
                                  );
                                }}
                              >
                                <Trash2 className="h-3.5 w-3.5" />
                              </Button>
                            </div>
                          </td>
                        </tr>
                      )
                    )}
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
                <Field label={t("models.apiKey")}>
                  <Input
                    required
                    type="password"
                    autoComplete="off"
                    placeholder={t("models.apiKeyPlaceholder")}
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
