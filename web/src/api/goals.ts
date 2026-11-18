import { api } from "@/lib/api";
import type { Goal, GoalProgress } from "@/api/types";

export type CreateGoalBody = {
  title: string;
  brief?: string;
  workspace_id: string;
  group_id?: string;
  budget_tokens?: number;
  risk?: string;
};

export function listGoals(limit = 100) {
  return api<{ goals: Goal[] }>(`/api/v1/goals?limit=${limit}`);
}

export function getGoal(id: string) {
  return api<{ goal: Goal; progress: GoalProgress }>(`/api/v1/goals/${encodeURIComponent(id)}`);
}

export function createGoal(body: CreateGoalBody) {
  return api<{ goal_id: string; status: string }>("/api/v1/goals", { method: "POST", body });
}

export function cancelGoal(id: string) {
  return api<Goal>(`/api/v1/goals/${encodeURIComponent(id)}/cancel`, { method: "POST" });
}
