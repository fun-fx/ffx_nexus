/**
 * LegacyRedirect — temporary escape hatch. The legacy 5-tab UI ships below
 * once /legacy/* URL is hit. Drop this entire folder after Pages migration
 * completes.
 */

import { Link } from "react-router-dom";
import { App as LegacyApp } from "../App.legacy";

export function LegacyRedirect() {
  return (
    <div style={{ padding: 24 }}>
      <p style={{ fontSize: 12, color: "var(--muted)", marginBottom: 16 }}>
        Legacy 5-tab interface (temporary).{" "}
        <Link to="/">← Back to new overview</Link>
      </p>
      <LegacyApp />
    </div>
  );
}
