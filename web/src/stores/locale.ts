import { create } from "zustand";
import { persist } from "zustand/middleware";

export type Locale = "zh" | "en";

export const SUPPORTED_LOCALES: Locale[] = ["zh", "en"];

// 没有存过选择时跟随浏览器：中文环境用中文，其余英文。
// 加新语言时在这里补一项，并在 i18n/index.tsx 注册字典。
function detectLocale(): Locale {
  if (typeof navigator === "undefined") return "zh";
  const tag = (navigator.languages?.[0] || navigator.language || "").toLowerCase();
  if (tag.startsWith("zh")) return "zh";
  if (tag.startsWith("en")) return "en";
  return tag ? "en" : "zh";
}

export function applyDocumentLang(locale: Locale) {
  if (typeof document !== "undefined") {
    document.documentElement.lang = locale === "zh" ? "zh-CN" : locale;
  }
}

type LocaleState = {
  locale: Locale;
  setLocale: (locale: Locale) => void;
};

export const useLocale = create<LocaleState>()(
  persist(
    (set) => ({
      locale: detectLocale(),
      setLocale: (locale) => {
        applyDocumentLang(locale);
        set({ locale });
      },
    }),
    { name: "janus-locale" }
  )
);

// 同步一次，之后只由 setLocale 触发；这样 Intl 格式与 <html lang> 永远一致。
applyDocumentLang(useLocale.getState().locale);
