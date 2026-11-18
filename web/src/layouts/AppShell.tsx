import { Suspense } from "react";
import { NavLink, Navigate, Outlet, useNavigate } from "react-router-dom";
import {
  BarChart3,
  Boxes,
  Gauge,
  LayoutDashboard,
  ListChecks,
  LogOut,
  Moon,
  Send,
  Sun,
  Target,
} from "lucide-react";
import { useAuth } from "@/stores/auth";
import { useTheme } from "@/stores/theme";
import { useI18n, type MessageKey } from "@/i18n";
import { cn } from "@/lib/cn";
import { Button } from "@/components/ui";
import { LanguageSwitcher } from "@/components/LanguageSwitcher";

const NAV: { to: string; key: MessageKey; icon: typeof LayoutDashboard; end?: boolean }[] = [
  { to: "/", key: "nav.dashboard", icon: LayoutDashboard, end: true },
  { to: "/goals", key: "nav.goals", icon: Target },
  { to: "/items", key: "nav.items", icon: ListChecks },
  { to: "/models", key: "nav.models", icon: Boxes },
  { to: "/usage", key: "nav.usage", icon: BarChart3 },
  { to: "/playground", key: "nav.playground", icon: Send },
  { to: "/scores", key: "nav.scores", icon: Gauge },
];

export function AppShell() {
  const key = useAuth((state) => state.key);
  const clear = useAuth((state) => state.clear);
  const theme = useTheme((state) => state.theme);
  const toggleTheme = useTheme((state) => state.toggle);
  const { t } = useI18n();
  const navigate = useNavigate();

  if (!key) return <Navigate to="/login" replace />;

  const mask = key.length > 10 ? `${key.slice(0, 4)}…${key.slice(-3)}` : t("common.unknown");

  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <aside className="hidden w-56 shrink-0 flex-col border-r bg-card md:flex">
        <div className="flex items-center gap-2 px-4 py-4">
          <span className="h-2.5 w-2.5 rounded-full bg-primary" />
          <span className="text-sm font-semibold">{t("app.name")}</span>
          <span className="text-xs text-muted-foreground">{t("app.console")}</span>
        </div>
        <nav className="flex flex-1 flex-col gap-1 px-2">
          {NAV.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) =>
                cn(
                  "flex items-center gap-2 rounded-lg px-3 py-2 text-sm",
                  isActive
                    ? "bg-muted font-medium text-foreground"
                    : "text-muted-foreground hover:bg-muted hover:text-foreground"
                )
              }
            >
              <item.icon className="h-4 w-4" />
              {t(item.key)}
            </NavLink>
          ))}
        </nav>
        <div className="border-t p-3 text-xs text-muted-foreground">
          <div className="mb-2 truncate font-mono">{mask}</div>
          <Button
            variant="ghost"
            className="w-full"
            onClick={() => {
              clear();
              navigate("/login", { replace: true });
            }}
          >
            <LogOut className="h-4 w-4" /> {t("nav.logout")}
          </Button>
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex items-center gap-2 border-b bg-card px-4 py-3 md:px-8">
          <nav className="flex flex-1 gap-1 overflow-x-auto md:hidden">
            {NAV.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                className={({ isActive }) =>
                  cn(
                    "whitespace-nowrap rounded-lg px-2.5 py-1.5 text-xs",
                    isActive ? "bg-muted font-medium" : "text-muted-foreground"
                  )
                }
              >
                {t(item.key)}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-2">
            <LanguageSwitcher compact />
            <Button variant="ghost" onClick={toggleTheme} aria-label={t("nav.theme")}>
              {theme === "dark" ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
            </Button>
            <Button
              variant="ghost"
              className="md:hidden"
              aria-label={t("nav.logout")}
              onClick={() => {
                clear();
                navigate("/login", { replace: true });
              }}
            >
              <LogOut className="h-4 w-4" />
            </Button>
          </div>
        </header>

        <main className="mx-auto w-full max-w-6xl flex-1 space-y-5 px-4 py-6 md:px-8">
          <Suspense fallback={<p className="text-sm text-muted-foreground">{t("common.loading")}</p>}>
            <Outlet />
          </Suspense>
        </main>
      </div>
    </div>
  );
}
