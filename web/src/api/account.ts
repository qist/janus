import { api } from "@/lib/api";
import type { Me } from "@/api/types";

/** Who the current key is, and what it may do. */
export function getMe() {
  return api<Me>("/api/v1/me");
}
