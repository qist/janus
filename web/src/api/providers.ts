import { api } from "@/lib/api";
import type {
  Channel,
  Credential,
  CredentialKind,
  DevicePoll,
  DeviceStart,
  OAuthConfig,
  OAuthStart,
  ProbeResult,
  Provider,
  ProviderKind,
  QuotaResult,
  Route,
  SyncResult,
} from "@/api/types";

export function listProviders() {
  return api<{ providers: Provider[] }>("/api/v1/providers");
}

export function listChannels() {
  return api<{ channels: Channel[] }>("/api/v1/channels");
}

export type CreateProviderBody = {
  id: string;
  name?: string;
  kind: ProviderKind;
  base_url: string;
  channel?: string;
  protocol?: "chat" | "responses" | "anthropic";
  oauth?: OAuthConfig;
};

export function createProvider(body: CreateProviderBody) {
  return api<Provider>("/api/v1/providers", { method: "POST", body });
}

export type ProviderPatch = {
  name?: string;
  base_url?: string;
  enabled?: boolean;
  protocol?: "chat" | "responses" | "anthropic";
  oauth?: OAuthConfig;
  clear_oauth?: boolean;
};

export function updateProvider(id: string, patch: ProviderPatch) {
  return api<Provider>(`/api/v1/providers/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: patch,
  });
}

export function deleteProvider(id: string) {
  return api<void>(`/api/v1/providers/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export function listCredentials(providerID: string) {
  return api<{ credentials: Credential[] }>(
    `/api/v1/providers/${encodeURIComponent(providerID)}/credentials`
  );
}

export type CreateCredentialBody = {
  label?: string;
  kind?: CredentialKind;
  secret: string;
};

export function createCredential(providerID: string, body: CreateCredentialBody) {
  return api<Credential>(`/api/v1/providers/${encodeURIComponent(providerID)}/credentials`, {
    method: "POST",
    body,
  });
}

export function deleteCredential(id: string) {
  return api<void>(`/api/v1/credentials/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export type CredentialPatch = {
  label?: string;
  account?: string;
  priority?: number;
  disabled?: boolean;
};

export function updateCredential(id: string, patch: CredentialPatch) {
  return api<Credential>(`/api/v1/credentials/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: patch,
  });
}

/** Clears cooldown/exhausted state after a top-up or an external re-auth. */
export function resetCredential(id: string) {
  return api<Credential>(`/api/v1/credentials/${encodeURIComponent(id)}/reset`, {
    method: "POST",
  });
}

export type ImportSource = "codex" | "claude" | "opencode";

export type ImportResult = {
  source: string;
  path: string;
  imported: Credential[];
  skipped?: string[];
};

/** Adopts a CLI sign-in that already exists on the server host. */
export function importLocal(
  providerID: string,
  body: { source: ImportSource; entry?: string; label?: string }
) {
  return api<ImportResult>(
    `/api/v1/providers/${encodeURIComponent(providerID)}/import`,
    { method: "POST", body }
  );
}

export function listRoutes() {
  return api<{ routes: Route[] }>("/api/v1/model-routes");
}

export type CreateRouteBody = {
  exposed_model: string;
  provider_id: string;
  upstream_model: string;
  aliases?: string[];
  priority?: number;
};

export function createRoute(body: CreateRouteBody) {
  return api<Route>("/api/v1/model-routes", { method: "POST", body });
}

export function deleteRoute(id: string) {
  return api<void>(`/api/v1/model-routes/${encodeURIComponent(id)}`, { method: "DELETE" });
}

/** Checks GET {base}/models with each stored credential; no tokens are spent. */
export function probeProvider(id: string) {
  return api<ProbeResult>(`/api/v1/providers/${encodeURIComponent(id)}/probe`, {
    method: "POST",
  });
}

/** Starts a PKCE sign-in and returns the vendor page to open. Passing a
 * credential id re-authorizes that account instead of adding a new one. */
export function startOAuth(providerID: string, credentialID?: string) {
  const suffix = credentialID ? `?credential_id=${encodeURIComponent(credentialID)}` : "";
  return api<OAuthStart>(
    `/api/v1/providers/${encodeURIComponent(providerID)}/oauth/start${suffix}`,
    { method: "POST" }
  );
}

/** Finishes a sign-in with the code (or full callback URL) the vendor showed. */
export function completeOAuth(
  providerID: string,
  sessionID: string,
  code: string,
  label?: string
) {
  return api<Credential>(`/api/v1/providers/${encodeURIComponent(providerID)}/oauth/complete`, {
    method: "POST",
    body: { session_id: sessionID, code, label },
  });
}

/** Starts a device-code sign-in (GitHub Copilot). */
export function startDevice(providerID: string, label?: string, credentialID?: string) {
  return api<DeviceStart>(`/api/v1/providers/${encodeURIComponent(providerID)}/device/start`, {
    method: "POST",
    body: { label, credential_id: credentialID },
  });
}

/** Polls a device sign-in once; status stays "pending" until the user approves. */
export function pollDevice(providerID: string, sessionID: string) {
  return api<DevicePoll>(`/api/v1/providers/${encodeURIComponent(providerID)}/device/poll`, {
    method: "POST",
    body: { session_id: sessionID },
  });
}

/** Compares the upstream /models with stored routes and creates what is missing.
 * prune=true also deletes this provider's routes whose model disappeared. */
export function syncRoutes(providerID: string, prune = false) {
  return api<SyncResult>(`/api/v1/providers/${encodeURIComponent(providerID)}/sync-routes`, {
    method: "POST",
    body: { prune },
  });
}

/** Reads the vendor's allowance/balance for every credential of one provider. */
export function refreshQuota(providerID: string) {
  return api<{ quotas: QuotaResult[] }>(
    `/api/v1/providers/${encodeURIComponent(providerID)}/quota`,
    { method: "POST" }
  );
}

/** Returns the stored quota snapshots without contacting any vendor. */
export function listQuotas(providerID?: string) {
  const suffix = providerID ? `?provider=${encodeURIComponent(providerID)}` : "";
  return api<{ quotas: QuotaResult[] }>(`/api/v1/quotas${suffix}`);
}
