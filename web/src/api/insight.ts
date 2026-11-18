import { api, queryString } from "@/lib/api";
import type { ModelInfo, Score, UsageTotals } from "@/api/types";

export function listModels() {
  return api<{ object: string; data: ModelInfo[] }>("/v1/models");
}

export type UsageFilter = {
  key?: string;
  group?: string;
  work_item?: string;
  provider?: string;
  model?: string;
  credential?: string;
  since?: number;
  until?: number;
};

export function getUsage(filter: UsageFilter = {}) {
  return api<UsageTotals>(`/api/v1/usage${queryString(filter)}`);
}

export function listScores() {
  return api<{ scores: Score[] }>("/api/v1/scores");
}
