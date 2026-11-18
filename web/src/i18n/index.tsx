import { useMemo } from "react";
import { useLocale, type Locale } from "@/stores/locale";
import { zh, type MessageKey } from "./zh";
import { en } from "./en";

export type { MessageKey, Locale };

export type Vars = Record<string, string | number>;

type Dictionary = Record<string, string>;

// 加语言的唯一入口：把字典登记在这里。页面只依赖 useI18n，不感知语言数量。
const DICTIONARIES: Partial<Record<Locale, Dictionary>> = { zh, en };

export const LANGUAGES: { code: Locale; label: string }[] = [
  { code: "zh", label: "中文" },
  { code: "en", label: "English" },
];

export function interpolate(template: string, vars?: Vars): string {
  if (!vars) return template;
  return template.replace(/\{(\w+)\}/g, (match, name: string) =>
    name in vars ? String(vars[name]) : match
  );
}

export type Translator = {
  (key: MessageKey, vars?: Vars): string;
  /** 动态键（如 status.<code>）；缺失时回落到 fallback。 */
  raw: (key: string, fallback: string, vars?: Vars) => string;
  has: (key: string) => boolean;
};

export function makeTranslator(locale: Locale): Translator {
  const dict: Dictionary = DICTIONARIES[locale] ?? zh;

  return Object.assign(
    (key: MessageKey, vars?: Vars) => interpolate(dict[key] ?? zh[key] ?? key, vars),
    {
      has: (key: string) => key in dict,
      raw: (key: string, fallback: string, vars?: Vars) => interpolate(dict[key] ?? fallback, vars),
    }
  );
}

export function useI18n() {
  const locale = useLocale((state) => state.locale);
  const setLocale = useLocale((state) => state.setLocale);
  const t = useMemo(() => makeTranslator(locale), [locale]);
  return { t, locale, setLocale, languages: LANGUAGES };
}
