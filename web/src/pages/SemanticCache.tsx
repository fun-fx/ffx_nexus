import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchGatewayConfig, patchGatewayConfig } from "../api";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { LabelToggle } from "../components/LabelToggle";
import { SettingRow } from "../components/SettingRow";

export function SemanticCache() {
  const qc = useQueryClient();
  const cfgQ = useQuery({ queryKey: ["gateway-config"], queryFn: fetchGatewayConfig });
  const sc = cfgQ.data?.semantic_cache;

  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [ttl, setTtl] = useState<string | null>(null);
  const [threshold, setThreshold] = useState<number | null>(null);
  const [maxEntries, setMaxEntries] = useState<number | null>(null);

  const saveMut = useMutation({
    mutationFn: () =>
      patchGatewayConfig({
        semantic_cache: {
          enabled: enabled ?? sc?.enabled,
          ttl: ttl ?? sc?.ttl,
          threshold: threshold ?? sc?.threshold,
          max_entries: maxEntries ?? sc?.max_entries,
        },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["gateway-config"] }),
  });

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Gateway · cache
          </div>
          <h1 className="page-title">
            <GradientText as="span">Semantic</GradientText> Cache
          </h1>
          <p className="page-sub">
            Embedding-similarity cache for deterministic requests. Requires Redis and an
            embeddings endpoint at boot.
          </p>
        </div>
      </header>

      {cfgQ.isLoading ? (
        <p className="muted">Loading…</p>
      ) : cfgQ.error ? (
        <Chip tone="err">{(cfgQ.error as Error).message}</Chip>
      ) : sc ? (
        <section className="panel panel-form">
          {!sc.redis_configured || !sc.embeddings_configured ? (
            <div className="banner warn" role="status">
              Redis: {sc.redis_configured ? "ok" : "missing"} · Embeddings:{" "}
              {sc.embeddings_configured ? "ok" : "missing"}. Enable via Helm env and
              restart to turn the cache on.
            </div>
          ) : null}
          <SettingRow
            label="Semantic cache enabled"
            hint="Return cached completions when a new prompt is similar to a prior one."
          >
            <LabelToggle
              checked={enabled ?? sc.enabled}
              label="semantic cache enabled"
              onChange={setEnabled}
            />
          </SettingRow>
          <label className="field-row">
            <span className="field-label">TTL</span>
            <span className="field-hint">Duration string, e.g. 24h.</span>
            <input value={ttl ?? sc.ttl} onChange={(e) => setTtl(e.target.value)} />
          </label>
          <label className="field-row">
            <span className="field-label">Similarity threshold</span>
            <span className="field-hint">Minimum embedding similarity (0–1) for a cache hit.</span>
            <input
              type="number"
              min={0}
              max={1}
              step={0.01}
              value={threshold ?? sc.threshold}
              onChange={(e) => setThreshold(Number(e.target.value))}
            />
          </label>
          <label className="field-row">
            <span className="field-label">Max entries per model</span>
            <span className="field-hint">Upper bound on cached prompts per model.</span>
            <input
              type="number"
              min={1}
              value={maxEntries ?? sc.max_entries}
              onChange={(e) => setMaxEntries(Number(e.target.value))}
            />
          </label>
          <div className="panel-form__actions">
            <button
              type="button"
              className="btn-neon"
              disabled={saveMut.isPending}
              onClick={() => saveMut.mutate()}
            >
              {saveMut.isPending ? "Saving…" : "Save cache settings"}
            </button>
            {saveMut.error ? <Chip tone="err">{(saveMut.error as Error).message}</Chip> : null}
          </div>
        </section>
      ) : null}
    </div>
  );
}
