import { useState } from "react";
import { Navigate, useNavigate } from "react-router-dom";
import { useAuth } from "@/stores/auth";
import { listItems } from "@/api/work";
import { Button, Card, ErrorBox, Field, Input } from "@/components/ui";

export default function Login() {
  const key = useAuth((state) => state.key);
  const setKey = useAuth((state) => state.setKey);
  const clear = useAuth((state) => state.clear);
  const navigate = useNavigate();

  const [value, setValue] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  if (key) return <Navigate to="/" replace />;

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    const trimmed = value.trim();
    if (!trimmed) {
      setError(new Error("请输入 API key"));
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
          <h1 className="text-lg font-semibold">Janus</h1>
          <span className="text-xs text-muted-foreground">控制台登录</span>
        </div>

        <form onSubmit={submit} className="space-y-3">
          <Field label="API key">
            <Input
              type="password"
              autoFocus
              autoComplete="off"
              placeholder="JANUS_API_KEY"
              value={value}
              onChange={(event) => setValue(event.target.value)}
            />
          </Field>
          {error ? <ErrorBox error={error} /> : null}
          <Button type="submit" className="w-full" disabled={busy}>
            {busy ? "验证中…" : "进入控制台"}
          </Button>
        </form>

        <p className="mt-4 text-xs leading-relaxed text-muted-foreground">
          key 只保存在本机浏览器（localStorage），服务端没有会话。不同权限的 key 看到的操作不同：
          建项/取消需要 <span className="font-mono">write</span>，
          强制派工需要 <span className="font-mono">admin</span>。
        </p>
      </Card>
    </div>
  );
}
