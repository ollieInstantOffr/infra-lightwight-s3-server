import type { ReactNode } from "react";
import { NavLink, useNavigate } from "react-router-dom";
import { useSession } from "../lib/session";

const navigation = [
  { to: "/", label: "Overview", adminOnly: false, end: true },
  { to: "/buckets", label: "Buckets", adminOnly: false, end: false },
  { to: "/credentials", label: "Credentials", adminOnly: true, end: false },
  { to: "/users", label: "Members", adminOnly: true, end: false },
];

export function Shell({ children }: { children: ReactNode }) {
  const { user, signOut } = useSession();
  const navigate = useNavigate();

  return (
    <div className="flex min-h-full flex-col">
      <header className="border-b border-border bg-surface-raised">
        <div className="mx-auto flex max-w-6xl flex-wrap items-center gap-x-6 gap-y-2 px-4 py-3">
          <span className="font-semibold tracking-tight">Object Storage</span>

          <nav className="flex items-center gap-1" aria-label="Main">
            {navigation
              .filter((item) => !item.adminOnly || user?.isAdmin)
              .map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.end}
                  className={({ isActive }) =>
                    `rounded-md px-3 py-1.5 text-sm transition-colors ${
                      isActive ? "bg-surface font-medium text-ink" : "text-ink-muted hover:text-ink"
                    }`
                  }
                >
                  {item.label}
                </NavLink>
              ))}
          </nav>

          <div className="ml-auto flex items-center gap-3 text-sm">
            <span className="text-ink-muted" title={user?.role}>
              {user?.email}
            </span>
            <button
              className="text-ink-muted underline underline-offset-2 hover:text-ink"
              onClick={() => {
                void signOut().then(() => navigate("/sign-in", { replace: true }));
              }}
            >
              Sign out
            </button>
          </div>
        </div>
      </header>

      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-8">{children}</main>
    </div>
  );
}
