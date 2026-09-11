import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchGatewayConfig, patchGatewayConfig } from "../api";
import { Chip } from "../components/Chip";
import { GradientText } from "../components/GradientText";

export function Alerting() {
  const qc = useQueryClient();
  const cfgQ = useQuery({ queryKey: ["gateway-config"], queryFn: fetchGatewayConfig });
  const alert = cfgQ.data?.alerting;

  const [webhook, setWebhook] = useState("");
  const [slack, setSlack] = useState("");
  const [cooldown, setCooldown] = useState<string | null>(null);
  const [editWebhook, setEditWebhook] = useState(false);
  const [editSlack, setEditSlack] = useState(false);

  const saveMut = useMutation({
    mutationFn: () =>
      patchGatewayConfig({
        alerting: {
          failover_webhook: editWebhook ? webhook : undefined,
          failover_slack: editSlack ? slack : undefined,
          cooldown: cooldown ?? alert?.cooldown,
        },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["gateway-config"] });
      setEditWebhook(false);
      setEditSlack(false);
      setWebhook("");
      setSlack("");
    },
  });

  return (
    <div className="page">
      <header className="page-head">
        <div>
          <div className="eyebrow">
            <span className="dot" aria-hidden="true" /> Gateway · failover alerts
          </div>
          <h1 className="page-title">
            <GradientText as="span">Alerting</GradientText>
          </h1>
          <p className="page-sub">
            Notify when routing failovers fire. Webhook URLs are stored in-process for
            this pod; set Helm env for persistence across restarts.
          </p>
        </div>
      </header>

      {cfgQ.isLoading ? (
        <p className="muted">Loading…</p>
      ) : cfgQ.error ? (
        <Chip tone="err">{(cfgQ.error as Error).message}</Chip>
      ) : alert ? (
        <section className="panel panel-form">
          <p className="panel-form__intro">
            When the gateway exhausts a routing tier and fails over to the next model,
            Nexus can POST a JSON payload to a generic webhook or Slack incoming webhook.
            Cooldown suppresses duplicate alerts for the same failover within the window.
          </p>

          <div className="panel-form__section">
            <h2 className="panel-form__section-title">Channels</h2>
            <div className="alerting-status">
              <Chip tone={alert.failover_webhook_set ? "ok" : "neutral"}>
                Generic webhook: {alert.failover_webhook_set ? "configured" : "not set"}
              </Chip>
              <Chip tone={alert.failover_slack_set ? "ok" : "neutral"}>
                Slack: {alert.failover_slack_set ? "configured" : "not set"}
              </Chip>
            </div>
            <label className="field-row">
              <span className="field-label">Failover webhook URL</span>
              <span className="field-hint">
                HTTPS endpoint that receives failover JSON. Leave blank to keep the current value.
              </span>
              <input
                type="url"
                placeholder={alert.failover_webhook_set ? "•••••••• (leave blank to keep)" : "https://…"}
                value={webhook}
                onChange={(e) => {
                  setWebhook(e.target.value);
                  setEditWebhook(true);
                }}
              />
            </label>
            <label className="field-row">
              <span className="field-label">Slack webhook URL</span>
              <span className="field-hint">
                Slack incoming webhook for failover notifications. Leave blank to keep the current value.
              </span>
              <input
                type="url"
                placeholder={
                  alert.failover_slack_set ? "•••••••• (leave blank to keep)" : "https://hooks.slack.com/…"
                }
                value={slack}
                onChange={(e) => {
                  setSlack(e.target.value);
                  setEditSlack(true);
                }}
              />
            </label>
          </div>

          <div className="panel-form__section">
            <h2 className="panel-form__section-title">Rate limit</h2>
            <label className="field-row">
              <span className="field-label">Alert cooldown</span>
              <span className="field-hint">
                Minimum time between alerts for the same failover. Use 0 or 0s to disable.
              </span>
              <input
                value={cooldown ?? alert.cooldown}
                onChange={(e) => setCooldown(e.target.value)}
                placeholder="0s"
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
              {saveMut.isPending ? "Saving…" : "Save alerting"}
            </button>
            {saveMut.error ? <Chip tone="err">{(saveMut.error as Error).message}</Chip> : null}
          </div>
        </section>
      ) : null}
    </div>
  );
}
