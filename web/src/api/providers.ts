import { api } from "@/lib/api";
import type { Credential, CredentialKind, Provider, ProviderKind, Route } from "@/api/types";

export function listProviders() {
  return api<{ providers: Provider[] }>("/api/v1/providers");
}

export type CreateProviderBody = {
  id: string;
  name?: string;
  kind: ProviderKind;
  base_url: string;
};

export function createProvider(body: CreateProviderBody) {
  return api<Provider>("/api/v1/providers", { method: "POST", body });
}

export type ProviderPatch = {
  name?: string;
  base_url?: string;
  enabled?: boolean;
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
