import { useEffect, useState } from "react";
import { listModels } from "@/api/insight";
import type { ModelInfo } from "@/api/types";
import { useI18n } from "@/i18n";
import { Card, CodeText, Empty, ErrorBox, Loading, PageHeader } from "@/components/ui";

export default function Models() {
  const { t } = useI18n();
  const [models, setModels] = useState<ModelInfo[] | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    listModels()
      .then((data) => setModels(data.data))
      .catch(setError);
  }, []);

  return (
    <>
      <PageHeader title={t("models.title")} description={t("models.description")} />

      {error ? <ErrorBox error={error} /> : null}

      <Card>
        {models === null ? (
          <Loading />
        ) : models.length === 0 ? (
          <Empty text={t("models.empty")} />
        ) : (
          <table className="w-full text-sm">
            <thead className="text-left text-xs text-muted-foreground">
              <tr>
                <th className="py-2">{t("models.modelAlias")}</th>
                <th>{t("models.type")}</th>
                <th>{t("models.owner")}</th>
              </tr>
            </thead>
            <tbody>
              {models.map((model) => (
                <tr key={model.id} className="border-t">
                  <td className="py-2 font-mono text-xs">{model.id}</td>
                  <td>{model.object}</td>
                  <td className="text-xs text-muted-foreground">{model.owned_by || t("common.none")}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      <Card>
        <h2 className="mb-2 text-sm font-semibold">{t("models.more")}</h2>
        <p className="text-sm text-muted-foreground">
          <CodeText text={t("models.moreBody")} />
        </p>
        <pre className="mt-3 overflow-auto rounded-lg bg-muted/50 p-3 font-mono text-xs">{`export JANUS_MODELS='[
  {"name":"openai","base_url":"https://api.openai.com/v1","model":"gpt-4o-mini",
   "aliases":["fast"],"api_key_envs":["OPENAI_KEY_1","OPENAI_KEY_2"]}
]'`}</pre>
      </Card>
    </>
  );
}
