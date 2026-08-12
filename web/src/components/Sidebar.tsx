import { NavLink } from "react-router-dom";
import { useEffect, useState } from "react";
import { Icon } from "./icons";
import { fetchMe, fetchUIObservability, type User } from "../api";

type NavItem = {
  to: string;
  label: string;
  icon: keyof typeof Icon;
  group: "Workspace" | "Admin";
};

const NAV: NavItem[] = [
  { to: "/", label: "Overview", icon: "grid", group: "Workspace" },
  { to: "/spend", label: "Spend", icon: "wallet", group: "Workspace" },
  { to: "/playground", label: "Playground", icon: "play", group: "Workspace" },
  { to: "/traces", label: "Traces", icon: "chart", group: "Workspace" },
  { to: "/routing", label: "Routing", icon: "zap", group: "Workspace" },
  { to: "/keys", label: "Keys", icon: "keys", group: "Workspace" },
  { to: "/credentials", label: "Credentials", icon: "shield", group: "Workspace" },
  { to: "/eval", label: "Eval", icon: "sparkles", group: "Admin" },
  { to: "/eval/benchmarks", label: "Benchmarks", icon: "chart", group: "Admin" },
  { to: "/audit", label: "Audit", icon: "list", group: "Admin" },
  { to: "/users", label: "Users", icon: "users", group: "Admin" },
];

// A row whose path is a prefix of another row's must match exactly, or
// both light up while the nested page is open.
function matchExactly(to: string): boolean {
  return to === "/" || NAV.some((n) => n.to !== to && n.to.startsWith(to + "/"));
}

export function Sidebar() {
  const [user, setUser] = useState<User | null>(null);
  const [grafana, setGrafana] = useState<string | null>(null);
  useEffect(() => {
    fetchMe()
      .then(setUser)
      .catch(() => setUser(null));
    // The Grafana URL is non-sensitive and anonymous, so we don't gate it
    // on user state. If the endpoint is empty (operator didn't set
    // NEXUS_PUBLIC_GRAFANA_URL) we just skip rendering the link entirely.
    fetchUIObservability()
      .then((o) => setGrafana(o.grafana?.base ?? null))
      .catch(() => setGrafana(null));
  }, []);

  const groups: Array<NavItem["group"]> = ["Workspace", "Admin"];
  const visibleItems = NAV.filter((n) => {
    if (n.group === "Admin") return user?.role === "admin";
    return true;
  });

  return (
    <aside className="sidebar" aria-label="Primary navigation">
      <div className="sidebar-brand">
        <span className="logo-mark" aria-hidden="true">
          ◆
        </span>
        <span className="brand-text">
          Nexus
          <span className="brand-sub">LLM Gateway</span>
        </span>
      </div>
      <nav className="sidebar-nav">
        {groups.map((g) => {
          const items = visibleItems.filter((i) => i.group === g);
          if (items.length === 0) return null;
          return (
            <div className="sidebar-group" key={g}>
              <div className="sidebar-group-label">{g}</div>
              {items.map((it) => {
                const IconC = Icon[it.icon];
                return (
                  <NavLink
                    key={it.to}
                    to={it.to}
                    end={matchExactly(it.to)}
                    className={({ isActive }) =>
                      "sidebar-item" + (isActive ? " is-active" : "")
                    }
                  >
                    <span className="sidebar-item-icon" aria-hidden="true">
                      <IconC size={16} />
                    </span>
                    <span className="sidebar-item-label">{it.label}</span>
                    <span className="sidebar-item-bar" aria-hidden="true" />
                  </NavLink>
                );
              })}
            </div>
          );
        })}
        {grafana ? (
          <div className="sidebar-group sidebar-external">
            <div className="sidebar-group-label">External</div>
            <a
              className="sidebar-item sidebar-item-external"
              href={grafana}
              target="_blank"
              rel="noreferrer"
            >
              <span className="sidebar-item-icon" aria-hidden="true">
                <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                  <path
                    d="M6 3h7v7M13 3 4 12"
                    stroke="currentColor"
                    strokeWidth="1.4"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                </svg>
              </span>
              <span className="sidebar-item-label">Open in Grafana</span>
              <span className="sidebar-item-bar" aria-hidden="true" />
            </a>
          </div>
        ) : null}
      </nav>
      <div className="sidebar-foot">
        {user ? (
          <div className="sidebar-user">
            <span className="avatar" aria-hidden="true">
              {user.email.slice(0, 1).toUpperCase()}
            </span>
            <span className="who" title={user.email}>
              {user.email}
              <span className="role">{user.role}</span>
            </span>
          </div>
        ) : (
          <a className="sidebar-cta" href="/login">
            Sign in →
          </a>
        )}
      </div>
    </aside>
  );
}
