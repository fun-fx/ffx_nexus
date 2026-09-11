import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchGatewayConfig, patchGatewayConfig } from "../api";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";
import { LabelToggle } from "../components/LabelToggle";
import { SettingRow } from "../components/SettingRow";

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
        <section className="panel panel-form">
          <p className="panel-form__intro">
            Guardrails are lightweight, in-process policy checks on the gateway hot path.
            They run before the upstream LLM call (input blocking) and after the response
            returns (output redaction and JSON validation). Unlike async evaluators, they
            can reject a request immediately — no external service or queue involved.
          </p>

          <div className="panel-form__section">
            <h2 className="panel-form__section-title">General</h2>
            <SettingRow
              label="Master switch"
              hint="Turn all guardrail checks on or off for this cluster."
            >
              <LabelToggle
                checked={enabled ?? g.enabled}
                label="guardrails master switch"
                onChange={setEnabled}
              />
            </SettingRow>
          </div>

          <div className="panel-form__section">
            <h2 className="panel-form__section-title">Input checks</h2>
            <SettingRow
              label="Block PII in input"
              hint="Reject prompts that match email, phone, SSN, or card-number patterns before any upstream call."
            >
              <LabelToggle
                checked={blockPii ?? g.block_pii_input}
                label="block PII in input"
                onChange={setBlockPii}
              />
            </SettingRow>
            <label className="field-row">
              <span className="field-label">Max input characters</span>
              <span className="field-hint">Reject prompts longer than this limit. Use 0 to disable.</span>
              <input
                type="number"
                min={0}
                value={maxChars ?? g.max_input_chars}
                onChange={(e) => setMaxChars(Number(e.target.value))}
              />
            </label>
            <label className="field-row">
              <span className="field-label">Deny patterns</span>
              <span className="field-hint">One regular expression per line. Matching prompts are rejected.</span>
              <textarea
                rows={4}
                value={denyRaw ?? (g.deny_patterns ?? []).join("\n")}
                onChange={(e) => setDenyRaw(e.target.value)}
                spellCheck={false}
              />
            </label>
          </div>

          <div className="panel-form__section">
            <h2 className="panel-form__section-title">Output checks</h2>
            <SettingRow
              label="Redact PII in output"
              hint="Replace detected PII in non-streaming responses with [REDACTED] instead of blocking the call."
            >
              <LabelToggle
                checked={redactPii ?? g.redact_pii_output}
                label="redact PII in output"
                onChange={setRedactPii}
              />
            </SettingRow>
            <SettingRow
              label="Validate JSON output"
              hint="When the client requests JSON response_format, reject or flag responses that are not valid JSON."
            >
              <LabelToggle
                checked={validateJson ?? g.validate_json_output}
                label="validate JSON output"
                onChange={setValidateJson}
              />
            </SettingRow>
            <SettingRow
              label="Structured-output self-correction"
              hint="If JSON validation fails, ask the model to repair the response before returning an error."
            >
              <LabelToggle
                checked={selfCorr ?? g.self_correction_enabled}
                label="structured-output self-correction"
                onChange={setSelfCorr}
              />
            </SettingRow>
            <label className="field-row">
              <span className="field-label">Self-correction max retries</span>
              <span className="field-hint">How many repair attempts before the gateway returns a guardrail error.</span>
              <input
                type="number"
                min={0}
                max={5}
                value={selfRetries ?? g.self_correction_max_retries}
                onChange={(e) => setSelfRetries(Number(e.target.value))}
              />
            </label>
          </div>

          <div className="panel-form__actions">
            <button
              type="button"
              className="btn-neon"
              disabled={saveMut.isPending}
              onClick={() => saveMut.mutate()}
            >
              {saveMut.isPending ? "Saving…" : "Save guardrails"}
            </button>
            {saveMut.error ? <Chip tone="err">{(saveMut.error as Error).message}</Chip> : null}
          </div>
        </section>
      ) : null}
    </div>
  );
}
