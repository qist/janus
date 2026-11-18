import type {
  ButtonHTMLAttributes,
  InputHTMLAttributes,
  ReactNode,
  SelectHTMLAttributes,
  TextareaHTMLAttributes,
} from "react";
import { useI18n } from "@/i18n";
import { cn } from "@/lib/cn";
import { ApiError } from "@/lib/api";
import { statusClass } from "@/lib/format";

type ButtonVariant = "default" | "ghost" | "danger";

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: ButtonVariant;
};

export function Button({ variant = "default", className, ...props }: ButtonProps) {
  return (
    <button
      className={cn(
        "inline-flex items-center justify-center gap-2 rounded-lg px-3 py-1.5 text-sm font-medium transition",
        "disabled:cursor-not-allowed disabled:opacity-50",
        variant === "default" && "bg-primary text-primary-foreground hover:opacity-90",
        variant === "ghost" && "border bg-transparent hover:bg-muted",
        variant === "danger" && "bg-danger text-white hover:opacity-90",
        className
      )}
      {...props}
    />
  );
}

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cn("input", className)} {...props} />;
}

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={cn("input", className)} {...props} />;
}

export function Select({ className, ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select className={cn("input", className)} {...props} />;
}

export function Field({
  label,
  children,
  className,
}: {
  label: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <label className={cn("flex flex-col gap-1.5", className)}>
      <span className="label">{label}</span>
      {children}
    </label>
  );
}

export function Badge({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium",
        className
      )}
    >
      {children}
    </span>
  );
}

export function StatusBadge({ status }: { status?: string }) {
  const { t } = useI18n();
  const label = status ? t.raw(`status.${status}`, status) : t("common.unknown");
  return <Badge className={statusClass(status)}>{label}</Badge>;
}

export function Card({ children, className }: { children: ReactNode; className?: string }) {
  return <section className={cn("card", className)}>{children}</section>;
}

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h1 className="text-xl font-semibold">{title}</h1>
        {description ? <p className="mt-1 text-sm text-muted-foreground">{description}</p> : null}
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </div>
  );
}

export function Empty({ text }: { text: string }) {
  return <p className="py-6 text-center text-sm text-muted-foreground">{text}</p>;
}

export function Loading() {
  const { t } = useI18n();
  return <p className="py-6 text-sm text-muted-foreground">{t("common.loading")}</p>;
}

export function ErrorBox({ error }: { error: unknown }) {
  const { t } = useI18n();
  const fallback = error instanceof Error ? error.message : String(error);
  const message =
    error instanceof ApiError ? t.raw(`error.${error.code}`, fallback) : fallback;

  return (
    <div className="rounded-lg border border-danger/40 bg-danger/10 px-4 py-3 text-sm text-danger">
      {message}
    </div>
  );
}

export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={cn("font-mono text-xs text-muted-foreground", className)}>{children}</span>;
}

// 文案里用反引号包住标识符，这里渲染成等宽样式，避免为每个语言拆多个键。
export function CodeText({ text, className }: { text: string; className?: string }) {
  const parts = text.split("`");
  return (
    <span className={className}>
      {parts.map((part, index) =>
        index % 2 === 1 ? (
          <Mono key={index} className="rounded bg-muted px-1 py-0.5">
            {part}
          </Mono>
        ) : (
          <span key={index}>{part}</span>
        )
      )}
    </span>
  );
}
