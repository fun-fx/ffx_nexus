import { NavLink, Outlet } from "react-router-dom";
import type { SectionTab } from "../nav/config";

export function SectionLayout({
  title,
  subtitle,
  tabs,
}: {
  title: string;
  subtitle?: string;
  tabs: SectionTab[];
}) {
  return (
    <div className="section-layout">
      <header className="section-layout-head">
        <div>
          <h1 className="page-title">{title}</h1>
          {subtitle ? <p className="page-sub">{subtitle}</p> : null}
        </div>
      </header>
      <nav className="section-tabs" aria-label={`${title} sections`}>
        {tabs.map((tab) => (
          <NavLink
            key={tab.to}
            to={tab.to}
            end={tab.end ?? !tab.to.includes("*")}
            className={({ isActive }) =>
              "section-tab" + (isActive ? " is-active" : "")
            }
          >
            {tab.label}
          </NavLink>
        ))}
      </nav>
      <div className="section-layout-body">
        <Outlet />
      </div>
    </div>
  );
}
