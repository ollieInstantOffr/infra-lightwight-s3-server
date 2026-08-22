import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { NavLink, useNavigate } from "react-router-dom";
import { useSession } from "../lib/session";
import { useApi } from "../lib/useApi";
import type { Dashboard } from "../lib/api";
import { formatBytes } from "../lib/format";
import { ProgressBar } from "./ui";
import { CommandPalette } from "./CommandPalette";

// The left rail from the design: brand and version at the top, navigation
// grouped by what it concerns, disk usage pinned to the bottom, and the
// signed-in user beneath that.

type NavItem = { to: string; label: string; adminOnly?: boolean; end?: boolean };
type NavGroup = { heading: string; items: NavItem[] };

const groups: NavGroup[] = [
  {
    heading: "Storage",
    items: [
      { to: "/", label: "Overview", end: true },
      { to: "/buckets", label: "Buckets" },
    ],
  },
  {
    heading: "Access",
    items: [
      { to: "/keys", label: "Access keys", adminOnly: true },
      { to: "/endpoint", label: "Endpoint & SDKs" },
      { to: "/users", label: "Users", adminOnly: true },
    ],
  },
  {
    heading: "Node",
    items: [
      { to: "/system", label: "System & health" },
      { to: "/audit", label: "Audit log", adminOnly: true },
    ],
  },
];

export function Shell({ children }: { children: ReactNode }) {
  const { user, signOut } = useSession();
  const navigate = useNavigate();
  const { data: dashboard } = useApi<Dashboard>("/api/dashboard");
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [railOpen, setRailOpen] = useState(false);

  // Cmd/Ctrl-K opens search from anywhere. Registered once, at the shell,
  // rather than per screen — a shortcut that only works on some pages is
  // worse than none.
  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setPaletteOpen(true);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const used = dashboard ? dashboard.diskTotal - dashboard.diskFree : 0;
  const usedFraction = dashboard && dashboard.diskTotal > 0 ? used / dashboard.diskTotal : 0;

  return (
    <div className="flex min-h-full">
      {/* On a narrow viewport the rail becomes a drawer, so the object browser
          keeps the full width it needs. */}
      <button
        className="fixed left-3 top-3 z-30 rounded-[10px] border border-line bg-card px-3 py-2 text-[13px] font-medium shadow-sm lg:hidden"
        onClick={() => setRailOpen((open) => !open)}
        aria-label="Toggle navigation"
      >
        ☰
      </button>

      {railOpen && (
        <div className="fixed inset-0 z-30 bg-[#12211d]/25 lg:hidden" onClick={() => setRailOpen(false)} />
      )}

      <aside
        className={`fixed inset-y-0 left-0 z-40 flex w-[236px] flex-none flex-col gap-[24px] border-r border-line bg-card px-[14px] py-[20px] transition-transform lg:static lg:translate-x-0 ${
          railOpen ? "translate-x-0" : "-translate-x-full"
        }`}
      >
        <div className="flex items-center gap-[9px] px-[6px]">
          <div className="flex size-[26px] flex-none items-center justify-center rounded-[8px] bg-accent font-mono text-[12px] font-semibold text-on-accent">
            P
          </div>
          <span className="text-[14px] font-semibold tracking-[-0.01em]">Pail</span>
          <span className="ml-auto rounded-[6px] border border-line bg-well px-[6px] py-[3px] font-mono text-[9.5px] font-medium text-ink-muted">
            {dashboard ? "on" : "…"}
          </span>
        </div>

        <nav className="flex flex-col gap-[2px]" aria-label="Main">
          {groups.map((group) => {
            const visible = group.items.filter((item) => !item.adminOnly || user?.isAdmin);
            if (visible.length === 0) return null;
            return (
              <div key={group.heading}>
                <div className="px-[8px] pb-[6px] pt-[16px] text-[10px] font-semibold uppercase tracking-[0.08em] text-ink-heading first:pt-0">
                  {group.heading}
                </div>
                {visible.map((item) => (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.end}
                    onClick={() => setRailOpen(false)}
                    className={({ isActive }) =>
                      `block rounded-[9px] px-[10px] py-[8px] text-[13px] font-medium transition-colors ${
                        isActive
                          ? "bg-accent-soft text-accent-deep"
                          : "text-ink-label hover:bg-inset hover:text-ink"
                      }`
                    }
                  >
                    {item.label}
                  </NavLink>
                ))}
              </div>
            );
          })}
        </nav>

        <div className="mt-auto flex flex-col gap-[12px]">
          <button
            onClick={() => setPaletteOpen(true)}
            className="flex items-center justify-between rounded-[10px] border border-line bg-inset px-[11px] py-[8px] text-[12px] text-ink-muted hover:text-ink"
          >
            <span>Search objects</span>
            <span className="font-mono text-[10px]">⌘K</span>
          </button>

          <div className="rounded-[12px] border border-[#e6efec] bg-inset p-[13px]">
            <div className="mb-[8px] flex justify-between text-[11px] text-ink-muted">
              <span>Disk</span>
              <span className="font-mono text-[11px] font-medium text-ink">
                {dashboard ? `${formatBytes(used)} / ${formatBytes(dashboard.diskTotal)}` : "—"}
              </span>
            </div>
            <ProgressBar
              fraction={usedFraction}
              tone={usedFraction > 0.9 ? "danger" : usedFraction > 0.85 ? "warn" : "accent"}
              label="Disk used"
            />
            <NavLink
              to="/system"
              className="mt-[10px] block text-[11.5px] font-semibold text-accent hover:text-accent-hover"
            >
              System &amp; health
            </NavLink>
          </div>

          <NavLink
            to="/account"
            className="flex items-center gap-[9px] rounded-[9px] p-[5px] hover:bg-inset"
            title="Your account"
          >
            <div className="flex size-[27px] flex-none items-center justify-center rounded-full bg-ink text-[11px] font-semibold text-[#eaf6f1]">
              {initials(user?.email ?? "")}
            </div>
            <div className="min-w-0">
              <div className="truncate text-[12px] font-semibold">{user?.email.split("@")[0]}</div>
              <div className="truncate text-[10.5px] text-ink-faint">{user?.email}</div>
            </div>
          </NavLink>

          <button
            className="px-[5px] text-left text-[11.5px] text-ink-faint hover:text-ink"
            onClick={() => {
              void signOut().then(() => navigate("/sign-in", { replace: true }));
            }}
          >
            Sign out
          </button>
        </div>
      </aside>

      <main className="min-w-0 flex-1 px-[32px] py-[28px] pb-[40px] max-lg:px-[18px] max-lg:pt-[60px]">
        <div className="mx-auto max-w-[1100px]">{children}</div>
      </main>

      {paletteOpen && <CommandPalette onClose={() => setPaletteOpen(false)} />}
    </div>
  );
}

/** Two letters from an address, for the avatar. */
function initials(email: string): string {
  const name = email.split("@")[0] ?? "";
  const parts = name.split(/[.\-_]/).filter(Boolean);
  if (parts.length >= 2) return (parts[0]![0]! + parts[1]![0]!).toUpperCase();
  return name.slice(0, 2).toUpperCase() || "?";
}
