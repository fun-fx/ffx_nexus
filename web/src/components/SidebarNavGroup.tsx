import { useEffect, useId, useState } from "react";
import { NavLink, useLocation } from "react-router-dom";
import { Icon } from "./icons";
import type { NavGroupDef, NavLinkItem } from "../nav/config";

const STORAGE_KEY = "nexus-nav-expanded";

function readExpanded(): Record<string, boolean> {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return {};
    return JSON.parse(raw) as Record<string, boolean>;
  } catch {
    return {};
  }
}

function writeExpanded(state: Record<string, boolean>) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
  } catch {
    /* ignore quota errors */
  }
}

function itemMatches(pathname: string, to: string, end?: boolean): boolean {
  if (end || to === "/") return pathname === to;
  return pathname === to || pathname.startsWith(to + "/");
}

function groupIsActive(pathname: string, group: NavGroupDef): boolean {
  if (group.to) return itemMatches(pathname, group.to, group.to === "/");
  return (group.items ?? []).some((it) => itemMatches(pathname, it.to, it.end));
}

function SidebarLink({ item }: { item: NavLinkItem }) {
  const IconC = item.icon ? Icon[item.icon] : null;
  return (
    <NavLink
      to={item.to}
      end={item.end ?? (item.to === "/" || !item.to.includes("/", 1))}
      className={({ isActive }) =>
        "sidebar-item sidebar-item-nested" + (isActive ? " is-active" : "")
      }
    >
      {IconC ? (
        <span className="sidebar-item-icon" aria-hidden="true">
          <IconC size={14} />
        </span>
      ) : null}
      <span className="sidebar-item-label">{item.label}</span>
      {item.badge === "soon" ? (
        <span className="sidebar-badge">soon</span>
      ) : null}
      <span className="sidebar-item-bar" aria-hidden="true" />
    </NavLink>
  );
}

export function SidebarNavGroup({ group }: { group: NavGroupDef }) {
  const location = useLocation();
  const panelId = useId();
  const active = groupIsActive(location.pathname, group);
  const [expanded, setExpanded] = useState(() => {
    const saved = readExpanded()[group.id];
    if (saved !== undefined) return saved;
    return active;
  });

  useEffect(() => {
    if (active) setExpanded(true);
  }, [active]);

  useEffect(() => {
    const all = readExpanded();
    all[group.id] = expanded;
    writeExpanded(all);
  }, [expanded, group.id]);

  if (group.to) {
    const IconC = Icon[group.icon];
    return (
      <NavLink
        to={group.to}
        end={group.to === "/"}
        className={({ isActive }) =>
          "sidebar-item" + (isActive ? " is-active" : "")
        }
      >
        <span className="sidebar-item-icon" aria-hidden="true">
          <IconC size={16} />
        </span>
        <span className="sidebar-item-label">{group.label}</span>
        <span className="sidebar-item-bar" aria-hidden="true" />
      </NavLink>
    );
  }

  const IconC = Icon[group.icon];
  const items = group.items ?? [];

  return (
    <div className={"sidebar-nav-group" + (expanded ? " is-expanded" : "")}>
      <button
        type="button"
        className={
          "sidebar-item sidebar-group-toggle" + (active ? " is-active-group" : "")
        }
        aria-expanded={expanded}
        aria-controls={panelId}
        onClick={() => setExpanded((v) => !v)}
      >
        <span className="sidebar-item-icon" aria-hidden="true">
          <IconC size={16} />
        </span>
        <span className="sidebar-item-label">{group.label}</span>
        <span className="sidebar-chevron" aria-hidden="true">
          ›
        </span>
        <span className="sidebar-item-bar" aria-hidden="true" />
      </button>
      <div id={panelId} className="sidebar-subnav" hidden={!expanded}>
        {items.map((item) => (
          <SidebarLink key={item.to} item={item} />
        ))}
      </div>
    </div>
  );
}
