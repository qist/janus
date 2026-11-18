import { useState } from "react";
import { Navigate, useNavigate } from "react-router-dom";
import { useAuth } from "@/stores/auth";
import { useI18n } from "@/i18n";
import { listItems } from "@/api/work";
import { Button, Card, ErrorBox, Field, Input } from "@/components/ui";
import { LanguageSwitcher } from "@/components/LanguageSwitcher";

export default function Login() {
  const key = useAuth((state) => state.key);
  const setKey = useAuth((state) => state.setKey);
  const clear = useAuth((state) => state.clear);
  const { t } = useI18n();
  const navigate = useNavigate();

  const [value, setValue] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  if (key) return <Navigate to="/" replace />;

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    const trimmed = value.trim();
    if (!trimmed) {
      setError(new Error(t("login.required")));
      return;
    }
    setBusy(true);
    setError(null);
    setKey(trimmed);
    try {
      // 用一个只读请求验证 key 是否有效；失败会清空并抛错。
      await listItems({ status: "open" });
      navigate("/", { replace: true });
    } catch (err) {
      clear();
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <Card className="w-full max-w-md">
        <div className="mb-5 flex items-center gap-2">
          <span className="h-2.5 w-2.5 rounded-full bg-primary" />
          <h1 className="text-lg font-semibold">{t("app.name")}</h1>
          <span className="text-xs text-muted-foreground">{t("login.title")}</span>
          <div className="ml-auto">
            <LanguageSwitcher compact />
          </div>
        </div>

        <form onSubmit={submit} className="space-y-3">
          <Field label={t("login.apiKey")}>
            <Input
              type="password"
              autoFocus
              autoComplete="off"
              placeholder={t("login.placeholder")}
              value={value}
              onChange={(event) => setValue(event.target.value)}
            />
          </Field>
          {error ? <ErrorBox error={error} /> : null}
          <Button type="submit" className="w-full" disabled={busy}>
            {busy ? t("login.verifying") : t("login.enter")}
          </Button>
        </form>

        <p className="mt-4 text-xs leading-relaxed text-muted-foreground">{t("login.hint")}</p>
      </Card>
    </div>
  );
}
