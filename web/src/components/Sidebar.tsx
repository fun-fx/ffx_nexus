import { useEffect, useState } from "react";
import { fetchMe, fetchUIObservability, type User } from "../api";
import { NAV_GROUPS } from "../nav/config";
import { SidebarNavGroup } from "./SidebarNavGroup";

export function Sidebar() {
  const [user, setUser] = useState<User | null>(null);
  const [grafana, setGrafana] = useState<string | null>(null);

  useEffect(() => {
    fetchMe()
      .then(setUser)
      .catch(() => setUser(null));
    fetchUIObservability()
      .then((o) => setGrafana(o.grafana?.base ?? null))
      .catch(() => setGrafana(null));
  }, []);

  const isAdmin = user?.role === "admin";

  const visibleGroups = NAV_GROUPS.filter((g) => {
    if (!g.adminOnly) return true;
    return isAdmin;
  }).map((g) => {
    if (!g.items) return g;
    return {
      ...g,
      items: g.items.filter((it) => !it.adminOnly || isAdmin),
    };
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
        {visibleGroups.map((group) => (
          <SidebarNavGroup key={group.id} group={group} />
        ))}
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
