import { useEffect, useState } from "react";
import { listModels } from "@/api/insight";
import type { ModelInfo } from "@/api/types";
import { Card, Empty, ErrorBox, Loading, Mono, PageHeader } from "@/components/ui";

export default function Models() {
  const [models, setModels] = useState<ModelInfo[] | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    listModels()
      .then((data) => setModels(data.data))
      .catch(setError);
  }, []);

  return (
    <>
      <PageHeader
        title="模型"
        description="对外暴露的模型与别名；上游端点、key 池由服务端 JANUS_MODELS 管理。"
      />

      {error ? <ErrorBox error={error} /> : null}

      <Card>
        {models === null ? (
          <Loading />
        ) : models.length === 0 ? (
          <Empty text="还没有暴露任何模型：设置 JANUS_MODELS 后重启。" />
        ) : (
          <table className="w-full text-sm">
            <thead className="text-left text-xs text-muted-foreground">
              <tr>
                <th className="py-2">模型 / 别名</th>
                <th>类型</th>
                <th>归属</th>
              </tr>
            </thead>
            <tbody>
              {models.map((model) => (
                <tr key={model.id} className="border-t">
                  <td className="py-2 font-mono text-xs">{model.id}</td>
                  <td>{model.object}</td>
                  <td className="text-xs text-muted-foreground">{model.owned_by || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      <Card>
        <h2 className="mb-2 text-sm font-semibold">接入更多模型</h2>
        <p className="text-sm text-muted-foreground">
          当前模型来源是启动配置 <Mono>JANUS_MODELS</Mono>，每项一个 OpenAI 兼容端点，
          <Mono>aliases</Mono> 暴露别名，<Mono>api_key_envs</Mono> 支持多 key 额度切换。
          控制台内的 provider / 凭据增删（含 OAuth 登录）需要后端 CRUD 与加密存储，属于下一阶段。
        </p>
        <pre className="mt-3 overflow-auto rounded-lg bg-muted/50 p-3 font-mono text-xs">{`export JANUS_MODELS='[
  {"name":"openai","base_url":"https://api.openai.com/v1","model":"gpt-4o-mini",
   "aliases":["fast"],"api_key_envs":["OPENAI_KEY_1","OPENAI_KEY_2"]}
]'`}</pre>
      </Card>
    </>
  );
}
