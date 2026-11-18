import { api, queryString } from "@/lib/api";
import type { ModelInfo, Score, UsageEntry, UsageStats, UsageTotals } from "@/api/types";

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
  period?: "24h" | "7d" | "30d";
  bucket?: "hour" | "day";
  limit?: number;
  top?: number;
};

export function getUsage(filter: UsageFilter = {}) {
  return api<UsageTotals>(`/api/v1/usage${queryString(filter)}`);
}

/** Totals plus by-model/provider/key breakdowns and a trend series. */
export function getUsageStats(filter: UsageFilter = {}) {
  return api<UsageStats>(`/api/v1/usage/stats${queryString(filter)}`);
}

/** Newest calls first; the console shows the tail of the ledger. */
export function getUsageLogs(filter: UsageFilter = {}) {
  return api<{ entries: UsageEntry[] }>(`/api/v1/usage/logs${queryString(filter)}`);
}

export function listScores() {
  return api<{ scores: Score[] }>("/api/v1/scores");
}
