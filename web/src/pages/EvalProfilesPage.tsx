import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
import { fetchEvalConfig, fetchMe } from "../api";
import { EvalProfilesCard } from "./EvalProfiles";
import { GradientText } from "../components/GradientText";

function Forbidden() {
  return (
    <div className="placeholder-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Admin · eval
          </div>
          <h1 className="page-title">
            <GradientText as="span">Forbidden</GradientText>
          </h1>
          <p className="page-sub">Only admin accounts can view this page.</p>
        </div>
      </header>
    </div>
  );
}

export function EvalProfilesPage() {
  const [searchParams] = useSearchParams();
  const openId = searchParams.get("open");

  const meQ = useQuery({ queryKey: ["me"], queryFn: fetchMe });
  const cfgQ = useQuery({
    queryKey: ["eval-config-profiles"],
    queryFn: () => fetchEvalConfig().catch(() => null),
  });

  const isAdmin = meQ.data?.role === "admin";
  const cfg = cfgQ.data;

  if (!isAdmin) return <Forbidden />;
  if (cfgQ.isLoading) {
    return (
      <div className="page-head">
        <p className="page-sub">Loading profiles…</p>
      </div>
    );
  }

  return (
    <div className="eval-page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Admin · eval profiles
          </div>
          <h1 className="page-title">
            <GradientText as="span">Eval</GradientText> profiles
          </h1>
          <p className="page-sub">
            Per-org heuristic, judge, and remote sidecar profiles.
          </p>
        </div>
      </header>
      <EvalProfilesCard
        isAdmin={isAdmin}
        pendingOpenProfileId={openId}
        hidden={cfg?.plugin_only ?? false}
      />
    </div>
  );
}
