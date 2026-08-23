import { useCallback, useEffect, useState } from "react";
import { alertsChanged, api, type Alert, type AlertPage, type AlertRule } from "../lib/api";
import { formatDate, formatRelative } from "../lib/format";
import { useSession } from "../lib/session";
import {
  Button,
  Card,
  EmptyState,
  ErrorNotice,
  InfoNotice,
  PageHeader,
  RowAction,
  Select,
  Spinner,
  Tabs,
  Tag,
  Toggle,
} from "../components/ui";

type Tab = "current" | "rules";

const severityTone = { critical: "danger", warning: "warn", info: "neutral" } as const;

export function AlertsPage() {
  const { user } = useSession();
  const [tab, setTab] = useState<Tab>("current");
  const [page, setPage] = useState<AlertPage | null>(null);
  const [showResolved, setShowResolved] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    api
      .get<AlertPage>(`/api/alerts${showResolved ? "?resolved=1" : ""}`)
      .then(setPage)
      .catch((caught: unknown) =>
        setError(caught instanceof Error ? caught.message : "Could not load alerts."),
      );
  }, [showResolved]);

  useEffect(load, [load]);

  async function act(alert: Alert, action: "acknowledge" | "resolve") {
    try {
      await api.post(`/api/alerts/${alert.id}/${action}`);
      load();
      alertsChanged();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not update the alert.");
    }
  }

  return (
    <>
      <PageHeader
        title="Alerts"
        subtitle={
          page
            ? page.firing + page.acknowledged === 0
              ? "Nothing needs attention."
              : `${page.firing} firing, ${page.acknowledged} acknowledged.`
            : undefined
        }
        actions={
          tab === "current" ? (
            <Select
              value={showResolved ? "1" : ""}
              onChange={(value) => setShowResolved(value === "1")}
              ariaLabel="Which alerts"
              options={[
                { value: "", label: "Needs attention" },
                { value: "1", label: "Include resolved" },
              ]}
            />
          ) : undefined
        }
      />

      {error && <ErrorNotice message={error} />}

      {user?.isAdmin && (
        <Tabs
          tabs={[
            { id: "current", label: "Current" },
            { id: "rules", label: "Rules" },
          ]}
          active={tab}
          onChange={setTab}
        />
      )}

      {tab === "rules" && user?.isAdmin ? (
        <AlertRules />
      ) : !page ? (
        <Spinner label="Loading alerts" />
      ) : page.alerts.length === 0 ? (
        <Card>
          <EmptyState
            title="Nothing needs attention"
            hint="Alerts appear here when the error rate rises, the disk fills, uploads start failing, or one credential is repeatedly rejected."
          />
        </Card>
      ) : (
        <div className="space-y-[12px]">
          {page.alerts.map((alert) => (
            <Card
              key={alert.id}
              className={`p-[17px] ${
                alert.state === "resolved"
                  ? "opacity-60"
                  : alert.severity === "critical"
                    ? "border-danger/30"
                    : ""
              }`}
            >
              <div className="flex flex-wrap items-start justify-between gap-[12px]">
                <div className="min-w-0">
                  <div className="mb-[5px] flex flex-wrap items-center gap-[7px]">
                    <Tag tone={severityTone[alert.severity]}>{alert.severity}</Tag>
                    <span className="text-[13px] font-semibold">{alert.ruleName}</span>
                    {alert.state === "acknowledged" && <Tag>acknowledged</Tag>}
                    {alert.state === "resolved" && <Tag tone="accent">resolved</Tag>}
                  </div>
                  <p className="m-0 text-[13px] leading-[1.6]">{alert.summary}</p>
                  {alert.guidance && (
                    <p className="m-0 mt-[7px] text-[12px] leading-[1.6] text-ink-muted">
                      {alert.guidance}
                    </p>
                  )}
                  <p className="m-0 mt-[8px] text-[11px] text-ink-faint">
                    Started {formatDate(alert.firedAt)} · last seen {formatRelative(alert.lastSeenAt)}
                    {alert.acknowledgedBy && ` · acknowledged by ${alert.acknowledgedBy}`}
                  </p>
                </div>

                {alert.state !== "resolved" && (
                  <div className="flex flex-none gap-[6px]">
                    {alert.state === "firing" && (
                      <Button onClick={() => void act(alert, "acknowledge")}>Acknowledge</Button>
                    )}
                    <RowAction onClick={() => void act(alert, "resolve")}>Dismiss</RowAction>
                  </div>
                )}
              </div>
            </Card>
          ))}

          <InfoNotice>
            Alerts resolve themselves when the condition clears. Acknowledging stops the email
            without hiding the alert — dismissing one whose condition still holds simply raises it
            again at the next evaluation.
          </InfoNotice>
        </div>
      )}
    </>
  );
}

function AlertRules() {
  const [rules, setRules] = useState<AlertRule[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);

  const load = useCallback(() => {
    api
      .get<{ rules: AlertRule[] }>("/api/alerts/rules")
      .then((result) => setRules(result.rules))
      .catch((caught: unknown) =>
        setError(caught instanceof Error ? caught.message : "Could not load rules."),
      );
  }, []);

  useEffect(load, [load]);

  async function save(rule: AlertRule, changes: Partial<AlertRule>) {
    const next = { ...rule, ...changes };
    try {
      await api.put(`/api/alerts/rules/${rule.id}`, {
        enabled: next.enabled,
        severity: next.severity,
        settings: next.settings ?? {},
      });
      setSaved(rule.id);
      window.setTimeout(() => setSaved(null), 1600);
      load();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not save the rule.");
    }
  }

  if (error) return <ErrorNotice message={error} />;
  if (!rules) return <Spinner label="Loading rules" />;

  return (
    <div className="space-y-[12px]">
      {rules.map((rule) => (
        <Card key={rule.id} className="p-[17px]">
          <div className="flex flex-wrap items-start justify-between gap-[14px]">
            <div className="min-w-0 flex-1">
              <Toggle
                checked={rule.enabled}
                onChange={(enabled) => void save(rule, { enabled })}
                label={rule.name}
                description={rule.description}
              />
              {rule.settings && Object.keys(rule.settings).length > 0 && (
                <div className="mt-[12px] flex flex-wrap gap-[16px] pl-[45px]">
                  {Object.entries(rule.settings).map(([key, value]) => (
                    <label key={key} className="block">
                      <span className="mb-[4px] block text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-heading">
                        {key.replace(/_/g, " ")}
                      </span>
                      <input
                        type="number"
                        defaultValue={value}
                        step="any"
                        className="w-[110px] rounded-[8px] border border-line-input bg-card px-[10px] py-[6px] font-mono text-[12px] outline-none focus:border-accent"
                        onBlur={(event) => {
                          const parsed = Number(event.target.value);
                          if (Number.isNaN(parsed) || parsed === value) return;
                          void save(rule, { settings: { ...rule.settings, [key]: parsed } });
                        }}
                      />
                    </label>
                  ))}
                </div>
              )}
            </div>

            <div className="flex flex-none items-center gap-[8px]">
              {saved === rule.id && <span className="text-[12px] text-ok">Saved</span>}
              <Select
                value={rule.severity}
                onChange={(severity) => void save(rule, { severity: severity as AlertRule["severity"] })}
                ariaLabel={`Severity for ${rule.name}`}
                options={[
                  { value: "info", label: "Info" },
                  { value: "warning", label: "Warning" },
                  { value: "critical", label: "Critical" },
                ]}
              />
            </div>
          </div>
        </Card>
      ))}

      <InfoNotice>
        Only warnings and critical alerts are emailed; info appears in the console alone. A still-firing
        alert re-notifies at most once every six hours, so a flapping condition sends one message rather
        than hundreds.
      </InfoNotice>
    </div>
  );
}
