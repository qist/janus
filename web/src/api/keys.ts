import { api } from "@/lib/api";
import type { APIKey, CreatedKey, KeyKind } from "@/api/types";

export function listKeys() {
  return api<{ keys: APIKey[] }>("/api/v1/keys");
}

export type CreateKeyBody = {
  kind: KeyKind;
  name?: string;
  notes?: string;
  expires_in_days?: number;
};

/** The response carries the only copy of the plaintext key. */
export function createKey(body: CreateKeyBody) {
  return api<CreatedKey>("/api/v1/keys", { method: "POST", body });
}

export function revokeKey(id: string) {
  return api<void>(`/api/v1/keys/${encodeURIComponent(id)}`, { method: "DELETE" });
}

/** Mints a replacement and revokes the old key in one call. */
export function rotateKey(id: string) {
  return api<CreatedKey>(`/api/v1/keys/${encodeURIComponent(id)}/rotate`, {
    method: "POST",
  });
}
