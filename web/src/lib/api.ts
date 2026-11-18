import { useAuth } from "@/stores/auth";

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

type RequestOptions = {
  method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  body?: unknown;
};

/**
 * 统一请求入口：同源、JSON、带 API key。
 * 401 清空本地 key 并跳登录页；403 保留 key（只是缺 scope）。
 */
export async function api<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const key = useAuth.getState().key;
  const headers: Record<string, string> = { Accept: "application/json" };
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  if (key) headers["X-API-Key"] = key;

  let response: Response;
  try {
    response = await fetch(path, {
      method: options.method ?? "GET",
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
    });
  } catch (error) {
    throw new ApiError(0, "network_error", error instanceof Error ? error.message : "网络错误");
  }

  const text = await response.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }

  if (!response.ok) {
    const detail = (data as { error?: { code?: string; message?: string } } | null)?.error;
    const code = detail?.code ?? String(response.status);
    const message = detail?.message ?? text ?? "请求失败";
    if (response.status === 401) {
      useAuth.getState().clear();
      if (!window.location.hash.startsWith("#/login")) {
        window.location.hash = "#/login";
      }
    }
    throw new ApiError(response.status, code, message);
  }

  if (response.status === 204) return undefined as T;
  return data as T;
}

export function queryString(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === "" || value === 0) continue;
    search.set(key, String(value));
  }
  const encoded = search.toString();
  return encoded ? `?${encoded}` : "";
}
