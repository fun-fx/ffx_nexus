import { NavLink } from "react-router-dom";
import { useEffect, useState } from "react";
import { Icon } from "./icons";
import { fetchMe, type User } from "../api";

type NavItem = {
  to: string;
  label: string;
  icon: keyof typeof Icon;
  group: "Workspace" | "Admin";
};

const NAV: NavItem[] = [
  { to: "/", label: "Overview", icon: "grid", group: "Workspace" },
  { to: "/playground", label: "Playground", icon: "play", group: "Workspace" },
  { to: "/traces", label: "Traces", icon: "chart", group: "Workspace" },
  { to: "/routing", label: "Routing", icon: "zap", group: "Workspace" },
  { to: "/keys", label: "Keys", icon: "keys", group: "Workspace" },
  { to: "/credentials", label: "Credentials", icon: "shield", group: "Workspace" },
  { to: "/eval", label: "Eval", icon: "sparkles", group: "Admin" },
  { to: "/audit", label: "Audit", icon: "list", group: "Admin" },
  { to: "/users", label: "Users", icon: "users", group: "Admin" },
];

export function Sidebar() {
  const [user, setUser] = useState<User | null>(null);
  useEffect(() => {
    fetchMe()
      .then(setUser)
      .catch(() => setUser(null));
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
                    end={it.to === "/"}
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
