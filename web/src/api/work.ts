import { api, queryString } from "@/lib/api";
import type { ClaimResult, ItemView, Role, WorkItem } from "@/api/types";

export function listItems(params: { status?: string; workspace?: string } = {}) {
  return api<{ items: WorkItem[] }>(`/api/v1/work-items${queryString(params)}`);
}

export function getItem(id: string) {
  return api<ItemView>(`/api/v1/work-items/${encodeURIComponent(id)}`);
}

export function createItem(item: WorkItem) {
  return api<{ id: string }>("/api/v1/work-items", { method: "POST", body: { item } });
}

export function cancelItem(id: string) {
  return api<void>(`/api/v1/work-items/${encodeURIComponent(id)}/cancel`, { method: "POST" });
}

export type AssignBody = {
  agent_id: string;
  role: Role;
  plan: string;
  lease_seconds?: number;
  model_id?: string;
  model_version?: string;
  tool_mode?: string;
  task_class?: string;
};

export function assignItem(id: string, body: AssignBody) {
  return api<ClaimResult>(`/api/v1/work-items/${encodeURIComponent(id)}/assign`, {
    method: "POST",
    body,
  });
}
