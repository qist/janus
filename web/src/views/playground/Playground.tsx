import { useCallback, useEffect, useRef, useState } from "react";
import { Play, Send, Square } from "lucide-react";
import { listModels } from "@/api/insight";
import type { ModelInfo } from "@/api/types";
import { useI18n } from "@/i18n";
import { ApiError } from "@/lib/api";
import { Button, Card, CodeText, Empty, ErrorBox, Field, Input, PageHeader, Select, Textarea } from "@/components/ui";
import { useAuth } from "@/stores/auth";

type Usage = {
  prompt_tokens?: number;
  completion_tokens?: number;
};

type Chunk = {
  choices?: { delta?: { content?: string | null }; finish_reason?: string | null }[];
  usage?: Usage;
};

/**
 * 控制台试跑面板：用当前 key 直接调 /v1/chat/completions，验证模型与渠道是否通。
 * 流式用 fetch + ReadableStream 自己解析，因为统一 api() 只处理一次性 JSON。
 */
export default function Playground() {
  const key = useAuth((state) => state.key);
  const { t } = useI18n();

  const [models, setModels] = useState<ModelInfo[]>([]);
  const [form, setForm] = useState({
    model: "",
    system: "",
    prompt: "",
    maxTokens: "256",
    temperature: "0.7",
    stream: true,
  });
  const [output, setOutput] = useState("");
  const [usage, setUsage] = useState<Usage | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [elapsed, setElapsed] = useState<number | null>(null);
  const abort = useRef<AbortController | null>(null);

  useEffect(() => {
    listModels()
      .then((data) => {
        setModels(data.data ?? []);
        setForm((current) =>
          current.model || !data.data?.length ? current : { ...current, model: data.data[0].id }
        );
      })
      .catch(() => setModels([]));
  }, []);

  const stop = useCallback(() => {
    abort.current?.abort();
    abort.current = null;
    setBusy(false);
  }, []);

  async function run() {
    if (!form.model || !form.prompt.trim()) return;
    const controller = new AbortController();
    abort.current = controller;
    setBusy(true);
    setError(null);
    setOutput("");
    setUsage(null);
    setElapsed(null);
    const started = performance.now();

    const messages: { role: string; content: string }[] = [];
    if (form.system.trim()) messages.push({ role: "system", content: form.system.trim() });
    messages.push({ role: "user", content: form.prompt });

    const body = {
      model: form.model,
      messages,
      stream: form.stream,
      max_tokens: Number(form.maxTokens) || undefined,
      temperature: form.temperature === "" ? undefined : Number(form.temperature),
    };

    try {
      const response = await fetch("/v1/chat/completions", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: form.stream ? "text/event-stream" : "application/json",
          ...(key ? { "X-API-Key": key } : {}),
        },
        body: JSON.stringify(body),
        signal: controller.signal,
      });

      if (!response.ok) {
        const text = await response.text();
        let detail: { error?: { code?: string; message?: string } } | null = null;
        try {
          detail = JSON.parse(text);
        } catch {
          detail = null;
        }
        throw new ApiError(
          response.status,
          detail?.error?.code ?? String(response.status),
          detail?.error?.message ?? text ?? t("common.failed")
        );
      }

      if (!form.stream || !response.body) {
        const raw = (await response.json()) as {
          choices?: { message?: { content?: string | null } }[];
          usage?: Usage;
        };
        setOutput(raw.choices?.[0]?.message?.content ?? "");
        setUsage(raw.usage ?? null);
      } else {
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          const frames = buffer.split("\n\n");
          buffer = frames.pop() ?? "";
          for (const frame of frames) {
            const line = frame.split("\n").find((item) => item.startsWith("data:"));
            if (!line) continue;
            const payload = line.slice(5).trim();
            if (!payload || payload === "[DONE]") continue;
            let chunk: Chunk;
            try {
              chunk = JSON.parse(payload) as Chunk;
            } catch {
              continue;
            }
            const delta = chunk.choices?.[0]?.delta?.content;
            if (delta) setOutput((current) => current + delta);
            if (chunk.usage) setUsage(chunk.usage);
          }
        }
      }
    } catch (err) {
      if (!(err instanceof DOMException && err.name === "AbortError")) setError(err);
    } finally {
      setElapsed(Math.round(performance.now() - started));
      abort.current = null;
      setBusy(false);
    }
  }

  return (
    <>
      <PageHeader title={t("playground.title")} description={t("playground.description")} />

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <div className="grid gap-3">
            <Field label={t("playground.model")}>
              <Select
                value={form.model}
                onChange={(event) => setForm({ ...form, model: event.target.value })}
              >
                {models.length === 0 ? <option value="">{t("playground.noModels")}</option> : null}
                {models.map((model) => (
                  <option key={model.id} value={model.id}>
                    {model.id}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label={t("playground.system")}>
              <Input
                value={form.system}
                placeholder={t("playground.systemPlaceholder")}
                onChange={(event) => setForm({ ...form, system: event.target.value })}
              />
            </Field>
            <Field label={t("playground.prompt")}>
              <Textarea
                rows={8}
                value={form.prompt}
                placeholder={t("playground.promptPlaceholder")}
                onChange={(event) => setForm({ ...form, prompt: event.target.value })}
              />
            </Field>
            <div className="grid gap-3 sm:grid-cols-3">
              <Field label={t("playground.maxTokens")}>
                <Input
                  type="number"
                  min={1}
                  value={form.maxTokens}
                  onChange={(event) => setForm({ ...form, maxTokens: event.target.value })}
                />
              </Field>
              <Field label={t("playground.temperature")}>
                <Input
                  type="number"
                  step="0.1"
                  min={0}
                  max={2}
                  value={form.temperature}
                  onChange={(event) => setForm({ ...form, temperature: event.target.value })}
                />
              </Field>
              <Field label={t("playground.stream")}>
                <Select
                  value={form.stream ? "1" : "0"}
                  onChange={(event) => setForm({ ...form, stream: event.target.value === "1" })}
                >
                  <option value="1">{t("common.yes")}</option>
                  <option value="0">{t("common.no")}</option>
                </Select>
              </Field>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Button disabled={busy || !form.model || !form.prompt.trim()} onClick={() => void run()}>
                <Send className="h-4 w-4" />
                {busy ? t("playground.running") : t("playground.send")}
              </Button>
              {busy ? (
                <Button variant="ghost" onClick={stop}>
                  <Square className="h-4 w-4" />
                  {t("playground.stop")}
                </Button>
              ) : null}
              {elapsed !== null ? (
                <span className="text-xs text-muted-foreground">
                  {t("playground.elapsed", { ms: elapsed })}
                  {usage?.prompt_tokens !== undefined
                    ? ` · ${usage.prompt_tokens} → ${usage.completion_tokens ?? 0} tokens`
                    : ""}
                </span>
              ) : null}
            </div>
          </div>
        </Card>

        <Card>
          <div className="mb-2 flex items-center justify-between">
            <h2 className="text-sm font-semibold">{t("playground.output")}</h2>
            {output ? (
              <Button variant="ghost" onClick={() => setOutput("")}>
                {t("playground.clear")}
              </Button>
            ) : null}
          </div>
          {error ? <ErrorBox error={error} /> : null}
          {!error && !output ? (
            <Empty text={busy ? t("playground.running") : t("playground.empty")} />
          ) : null}
          {output ? (
            <div className="space-y-3">
              <CodeText text={output} className="max-h-[28rem] overflow-auto whitespace-pre-wrap" />
              {usage ? (
                <p className="text-xs text-muted-foreground">
                  prompt {usage.prompt_tokens ?? 0} · completion {usage.completion_tokens ?? 0}
                </p>
              ) : null}
            </div>
          ) : null}
        </Card>
      </div>

      <p className="mt-4 flex items-center gap-2 text-xs text-muted-foreground">
        <Play className="h-3 w-3" />
        {t("playground.hint")}
      </p>
    </>
  );
}
