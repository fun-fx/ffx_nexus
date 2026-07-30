import { useEffect, useMemo, useRef, useState } from "react";
import {
  type PluginKeysEntry,
  type PluginKeysState,
  deletePluginKeys,
  fetchPluginKeys,
  putPluginKeys,
} from "../api";
import { Icon } from "./icons";

/**
 * Modal that lets an admin paste in the API keys for a single plugin.
 * Mounted by EvalPlugins.tsx via per-row "Keys" button.
 *
 * Behaviour:
 *   - On open: GET /api/eval/plugins/{name}/keys
 *     → returns which keys are *required* (from the manifest's keyRef)
 *       and whether each currently has a value configured.
 *   - On save: PUT /api/eval/plugins/{name}/keys
 *     → replaces the stored values. Empty inputs are stripped server-side.
 *   - On clear: DELETE /api/eval/plugins/{name}/keys
 *     → wipes stored values.
 *
 * Inputs are masked at all times. The mod view shows previews like
 * "pk-lf-***1234" so two Langfuse keys remain distinguishable.
 */
export function PluginKeysModal({
  pluginName,
  open,
  onClose,
}: {
  pluginName: string;
  open: boolean;
  onClose: () => void;
}) {
  // Server-truth loaded on open; null means "still loading or errored".
  const [state, setState] = useState<PluginKeysState | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  // Map keyed by key name. Values are raw user input. Empty == cleared.
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [showSecrets, setShowSecrets] = useState(false);
  const loadSeq = useRef(0);

  // List of keys to render. Comes from the response when present, so
  // unknown stored keys (from an older manifest revision) still show
  // up; falls back to required_key_names when keys[] is absent.
  const keysToRender = useMemo<PluginKeysEntry[]>(() => {
    if (!state) return [];
    if (state.keys && state.keys.length > 0) return state.keys;
    return (state.required_key_names ?? []).map((name) => ({ name, set: false }));
  }, [state]);

  useEffect(() => {
    if (!open || !pluginName) return;
    const seq = ++loadSeq.current;
    setLoadError(null);
    setDraft({});
    setSubmitError(null);
    setShowSecrets(false);
    fetchPluginKeys(pluginName)
      .then((s) => {
        if (seq !== loadSeq.current) return;
        setState(s);
        setDraft({});
      })
      .catch((err) => {
        if (seq !== loadSeq.current) return;
        setLoadError((err as Error).message);
        setState(null);
      });
  }, [open, pluginName]);

  if (!open) return null;

  const submit = async () => {
    if (!pluginName) return;
    setSubmitError(null);
    // Strip empties so we PUT only what the operator typed.
    const cleaned: Record<string, string> = {};
    for (const [k, v] of Object.entries(draft)) {
      if (v && v.trim() !== "") cleaned[k] = v.trim();
    }
    if (state?.configured && Object.keys(cleaned).length === 0) {
      // Operator blurred out every input without typing — assume
      // "clear", not "no change". Surfacing the clear explicitly is
      // safer than the silent no-op.
      try {
        const r = await deletePluginKeys(pluginName);
        setState(r);
        setDraft({});
        onClose();
      } catch (err) {
        setSubmitError((err as Error).message);
      }
      return;
    }
    if (Object.keys(cleaned).length === 0) {
      setSubmitError("No keys to save — paste at least one value first.");
      return;
    }
    try {
      await putPluginKeys(pluginName, cleaned);
      const refreshed = await fetchPluginKeys(pluginName);
      setState(refreshed);
      setDraft({});
      onClose();
    } catch (err) {
      setSubmitError((err as Error).message);
    }
  };

  const onClear = async () => {
    if (!pluginName) return;
    setSubmitError(null);
    try {
      const r = await deletePluginKeys(pluginName);
      setState(r);
      setDraft({});
      onClose();
    } catch (err) {
      setSubmitError((err as Error).message);
    }
  };

  return (
    <div className="modal-backdrop" onClick={onClose} data-testid="plugin-keys-backdrop">
      <div
        className="modal plugin-keys-modal"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-label={`API keys for ${pluginName}`}
        data-testid={`plugin-keys-modal-${pluginName}`}
      >
        <div className="modal-head">
          <h3>API keys — {pluginName}</h3>
          <button
            type="button"
            className="btn-ghost btn-small"
            onClick={onClose}
            aria-label="Close"
          >
            <Icon.x size={12} />
          </button>
        </div>

        {loadError ? (
          <p className="error small">{loadError}</p>
        ) : null}

        {!loadError && state ? (
          <>
            <p className="muted small">
              Values are encrypted with the Nexus master key and stored in the
              control-plane database, so they survive a restart or a deploy.
              They are never shown again after saving; rotation works by{" "}
              <strong>pasting the new values</strong> here.
            </p>
            <ul className="plugin-keys-list">
              {keysToRender.map((k) => (
                <li key={k.name} className="plugin-keys-row">
                  <label className="plugin-keys-row-label" htmlFor={`key-${pluginName}-${k.name}`}>
                    <code>{k.name}</code>
                    {k.set ? (
                      <span className="chip chip-ok small">configured</span>
                    ) : (
                      <span className="chip chip-warn small">missing</span>
                    )}
                  </label>
                  <input
                    id={`key-${pluginName}-${k.name}`}
                    type={showSecrets ? "text" : "password"}
                    autoComplete="off"
                    spellCheck={false}
                    className="plugin-keys-input"
                    placeholder={k.set ? "(already set — paste to rotate)" : `Paste your ${k.name}`}
                    value={draft[k.name] ?? ""}
                    onChange={(e) =>
                      setDraft((d) => ({ ...d, [k.name]: e.target.value }))
                    }
                    data-testid={`plugin-keys-input-${pluginName}-${k.name}`}
                  />
                </li>
              ))}
            </ul>
            <div className="plugin-keys-toolbar">
              <label className="plugin-keys-showsmall">
                <input
                  type="checkbox"
                  checked={showSecrets}
                  onChange={(e) => setShowSecrets(e.target.checked)}
                />
                Show values (do this on a private screen)
              </label>
              {state.configured ? (
                <button
                  type="button"
                  className="btn-ghost btn-small row-action-danger"
                  onClick={onClear}
                  data-testid={`plugin-keys-clear-${pluginName}`}
                >
                  Clear all keys
                </button>
              ) : null}
            </div>
            {submitError ? <p className="error small">{submitError}</p> : null}
            <div className="modal-foot">
              <button type="button" className="btn-ghost" onClick={onClose}>
                Cancel
              </button>
              <button
                type="button"
                className="btn-primary"
                onClick={submit}
                data-testid={`plugin-keys-save-${pluginName}`}
              >
                Save keys
              </button>
            </div>
          </>
        ) : !loadError ? (
          <p className="muted small">Loading…</p>
        ) : null}
      </div>
    </div>
  );
}
