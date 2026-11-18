import { Languages } from "lucide-react";
import { useI18n, type Locale } from "@/i18n";

// 语言选择器只改本地状态；没有服务端偏好，所以刷新后依然生效（localStorage）。
export function LanguageSwitcher({ compact = false }: { compact?: boolean }) {
  const { locale, setLocale, languages, t } = useI18n();

  return (
    <label
      className="inline-flex items-center gap-1.5"
      title={t("nav.language")}
      aria-label={t("nav.language")}
    >
      <Languages className="h-4 w-4 text-muted-foreground" />
      <select
        value={locale}
        onChange={(event) => setLocale(event.target.value as Locale)}
        className={
          compact
            ? "rounded-lg border bg-background px-2 py-1 text-xs outline-none focus:ring-2 focus:ring-primary/30"
            : "input h-8 w-24 py-0 text-xs"
        }
      >
        {languages.map((language) => (
          <option key={language.code} value={language.code}>
            {language.label}
          </option>
        ))}
      </select>
    </label>
  );
}
