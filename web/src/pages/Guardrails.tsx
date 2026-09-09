import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchGatewayConfig, patchGatewayConfig } from "../api";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { LabelToggle } from "../components/LabelToggle";

export function Guardrails() {
  const qc = useQueryClient();
  const cfgQ = useQuery({ queryKey: ["gateway-config"], queryFn: fetchGatewayConfig });
  const g = cfgQ.data?.guardrails;

  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [blockPii, setBlockPii] = useState<boolean | null>(null);
  const [redactPii, setRedactPii] = useState<boolean | null>(null);
  const [maxChars, setMaxChars] = useState<number | null>(null);
  const [denyRaw, setDenyRaw] = useState<string | null>(null);
  const [validateJson, setValidateJson] = useState<boolean | null>(null);
  const [selfCorr, setSelfCorr] = useState<boolean | null>(null);
  const [selfRetries, setSelfRetries] = useState<number | null>(null);

  useEffect(() => {
    if (!g) return;
    setEnabled(null);
    setBlockPii(null);
    setRedactPii(null);
    setMaxChars(null);
    setDenyRaw(null);
    setValidateJson(null);
    setSelfCorr(null);
    setSelfRetries(null);
  }, [g?.enabled, g?.block_pii_input]);

  const saveMut = useMutation({
    mutationFn: () =>
      patchGatewayConfig({
        guardrails: {
          enabled: enabled ?? g?.enabled,
          block_pii_input: blockPii ?? g?.block_pii_input,
          redact_pii_output: redactPii ?? g?.redact_pii_output,
          max_input_chars: maxChars ?? g?.max_input_chars,
          deny_patterns: (denyRaw ?? (g?.deny_patterns ?? []).join("\n"))
            .split("\n")
            .map((s) => s.trim())
            .filter(Boolean),
          validate_json_output: validateJson ?? g?.validate_json_output,
          self_correction_enabled: selfCorr ?? g?.self_correction_enabled,
          self_correction_max_retries: selfRetries ?? g?.self_correction_max_retries,
        },
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["gateway-config"] }),
  });

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Gateway · inline policy
          </div>
          <h1 className="page-title">
            <GradientText as="span">Guardrails</GradientText>
          </h1>
          <p className="page-sub">
            Synchronous hot-path checks before and after upstream calls. Changes apply
            immediately on the gateway worker.
          </p>
        </div>
      </header>

      {cfgQ.isLoading ? (
        <p className="muted">Loading…</p>
      ) : cfgQ.error ? (
        <Chip tone="err">{(cfgQ.error as Error).message}</Chip>
      ) : g ? (
        <section className="panel" style={{ padding: "1.25rem" }}>
          <LabelToggle
            checked={enabled ?? g.enabled}
            label="guardrails master switch"
            onChange={setEnabled}
          />
          <LabelToggle
            checked={blockPii ?? g.block_pii_input}
            label="block PII in input"
            onChange={setBlockPii}
          />
          <LabelToggle
            checked={redactPii ?? g.redact_pii_output}
            label="redact PII in output"
            onChange={setRedactPii}
          />
          <label className="field">
            <span>Max input characters (0 = off)</span>
            <input
              type="number"
              min={0}
              value={maxChars ?? g.max_input_chars}
              onChange={(e) => setMaxChars(Number(e.target.value))}
            />
          </label>
          <label className="field">
            <span>Deny patterns (one regex per line)</span>
            <textarea
              rows={4}
              value={denyRaw ?? (g.deny_patterns ?? []).join("\n")}
              onChange={(e) => setDenyRaw(e.target.value)}
              spellCheck={false}
            />
          </label>
          <LabelToggle
            checked={validateJson ?? g.validate_json_output}
            label="validate JSON output"
            onChange={setValidateJson}
          />
          <LabelToggle
            checked={selfCorr ?? g.self_correction_enabled}
            label="structured-output self-correction"
            onChange={setSelfCorr}
          />
          <label className="field">
            <span>Self-correction max retries</span>
            <input
              type="number"
              min={0}
              max={5}
              value={selfRetries ?? g.self_correction_max_retries}
              onChange={(e) => setSelfRetries(Number(e.target.value))}
            />
          </label>
          <button
            type="button"
            className="btn-neon"
            disabled={saveMut.isPending}
            onClick={() => saveMut.mutate()}
          >
            {saveMut.isPending ? "Saving…" : "Save guardrails"}
          </button>
          {saveMut.error ? <Chip tone="err">{(saveMut.error as Error).message}</Chip> : null}
        </section>
      ) : null}
    </div>
  );
}
