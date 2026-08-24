import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  api,
  type Alert,
  type AlertPage,
  type PerformanceInFlightEntry,
  type PerformanceLiveSnapshot,
  type PerformanceRange,
  type PerformanceSummary,
  type ServerEvent,
  type SlowestOperation,
} from "../lib/api";
import { useApi } from "../lib/useApi";
import { formatBytes, formatDate } from "../lib/format";
import {
  Button,
  Card,
  EmptyState,
  ErrorNotice,
  InfoNotice,
  PageHeader,
  Spinner,
  Tag,
} from "../components/ui";

// Request volume, latency and the slowest operations — the numbers a
// benchmark or an incident actually needs, each one linking straight into
// the log that produced it. Live mode streams the last minute at one-second
// resolution from in-memory state; everything else is windowed history.

type ViewMode = "live" | PerformanceRange;

export function PerformancePage() {
  const [mode, setMode] = useState<ViewMode>("live");

  return (
    <>
      <PageHeader
        title="Performance"
        subtitle={
          mode === "live"
            ? "Streaming live · 1s counters, straight off the in-memory collector"
            : "Every number here opens the log that produced it"
        }
        actions={<RangeSwitch mode={mode} onChange={setMode} />}
      />

      {mode === "live" ? <LiveView /> : <HistoricView range={mode} />}
    </>
  );
}

function RangeSwitch({ mode, onChange }: { mode: ViewMode; onChange: (mode: ViewMode) => void }) {
  const options: { value: ViewMode; label: string }[] = [
    { value: "live", label: "Live" },
    { value: "1h", label: "1h" },
    { value: "24h", label: "24h" },
    { value: "7d", label: "7d" },
  ];
  return (
    <div className="flex overflow-hidden rounded-[10px] border border-line bg-card">
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          onClick={() => onChange(option.value)}
          className={`flex items-center gap-[7px] border-l border-line px-[13px] py-[8px] text-[12.5px] font-semibold first:border-l-0 ${
            mode === option.value ? "bg-ink text-accent-soft" : "text-ink-muted hover:bg-well"
          }`}
        >
          {option.value === "live" && (
            <span
              className={`size-[7px] flex-none rounded-full ${mode === "live" ? "bg-accent-light" : "bg-line-input"}`}
            />
          )}
          {option.label}
        </button>
      ))}
    </div>
  );
}

// ─── Historic ────────────────────────────────────────────────────────────────

function HistoricView({ range }: { range: PerformanceRange }) {
  const { data, error, loading, reload } = useApi<PerformanceSummary>(`/api/performance?range=${range}`);

  function exportCSV() {
    if (!data) return;
    downloadCSV(data, range);
  }

  return (
    <>
      {error && <ErrorNotice message={error} onRetry={reload} />}

      <div className="mb-[13px] flex justify-end">
        <Button onClick={exportCSV} disabled={!data}>
          Export CSV
        </Button>
      </div>

      <div className="mb-[13px] grid grid-cols-4 gap-[13px]">
        <KPI label="Requests" value={loading ? null : compact(data?.requests ?? 0)} hint={rangeLabel(range)} />
        <KPI
          label="Error rate"
          value={loading ? null : `${((data?.errorRate ?? 0) * 100).toFixed(2)}%`}
          hint={loading ? undefined : `${compact((data?.clientErrors ?? 0) + (data?.serverErrors ?? 0))} failures`}
          tone={data && data.errorRate > 0.05 ? "danger" : undefined}
        />
        <KPI
          label="p99 latency"
          value={loading ? null : `${data?.latency.p99Ms ?? 0} ms`}
          hint={
            loading
              ? undefined
              : `${compact(data?.latency.overThreshold ?? 0)} over ${data?.slowThresholdMs ?? 0}ms`
          }
        />
        <KPI
          label="Max latency"
          value={loading ? null : `${data?.latency.maxMs ?? 0} ms`}
          hint={loading ? undefined : `${compact(data?.latency.sampleRows ?? 0)} sampled rows`}
        />
      </div>

      <div className="mb-[13px] flex items-start gap-[13px]">
        <Card className="min-w-0 flex-[1.9] p-[18px]">
          <div className="mb-[15px] flex items-baseline justify-between">
            <span className="text-[13.5px] font-semibold">Requests by outcome</span>
            <span className="text-[11.5px] text-ink-faint">{rangeLabel(range)}</span>
          </div>
          {loading || !data ? (
            <div className="flex h-[170px] items-center justify-center">
              <Spinner label="Loading" />
            </div>
          ) : (
            <RequestsChart series={data.series} />
          )}
        </Card>

        <Card className="min-w-0 flex-1 p-[18px]">
          <div className="mb-[4px] text-[13.5px] font-semibold">Latency</div>
          <p className="mb-[14px] text-[11.5px] leading-[1.5] text-ink-faint">
            From sampled request logs, not the rollups.
          </p>
          {loading || !data ? (
            <Spinner label="Loading" />
          ) : (
            <LatencyPanel summary={data} />
          )}
        </Card>
      </div>

      {data?.coverage.partial && (
        <InfoNotice tone="warn">
          The sampled log does not reach back to the start of this window — latency and the slowest
          operations below only cover since {formatDate(data.coverage.coveredSince)}. Requests and error
          rate are exact regardless: they come from the durable rollup, not the sample.
        </InfoNotice>
      )}

      <div className="mt-[13px] flex items-start gap-[13px]">
        <Card className="min-w-0 flex-[1.5] overflow-hidden">
          <div className="flex items-center gap-[10px] border-b border-line-row px-[17px] py-[15px]">
            <span className="text-[13.5px] font-semibold">Slowest operations</span>
            <span className="ml-auto text-[11.5px] text-ink-faint">click a row to open its requests</span>
          </div>
          {loading || !data ? (
            <div className="p-[17px]">
              <Spinner label="Loading" />
            </div>
          ) : data.slowestOperations.length === 0 ? (
            <EmptyState title="No slow requests in this window" hint="Nothing has crossed the sample log's radar." />
          ) : (
            <SlowestOperationsTable operations={data.slowestOperations} range={range} />
          )}
        </Card>

        <SidePanels />
      </div>
    </>
  );
}

function RequestsChart({ series }: { series: PerformanceSummary["series"] }) {
  const peak = Math.max(...series.map((p) => p.requests), 1);
  return (
    <>
      <div className="flex h-[170px] items-end gap-[3px]">
        {series.map((point) => {
          const height = Math.max((point.requests / peak) * 100, point.requests > 0 ? 4 : 1);
          const errorShare = point.requests > 0 ? point.errors / point.requests : 0;
          return (
            <div
              key={point.at}
              className={`flex-1 rounded-t-[2px] ${errorShare > 0.05 ? "bg-danger" : "bg-accent"}`}
              style={{ height: `${height}%` }}
              title={`${formatDate(point.at)}: ${point.requests.toLocaleString()} requests, ${point.errors.toLocaleString()} errors`}
            />
          );
        })}
      </div>
      <div className="mt-[9px] flex justify-between text-[11.5px] text-ink-faint">
        <span>{series[0] ? formatDate(series[0].at) : ""}</span>
        <span>{series.at(-1) ? formatDate(series.at(-1)!.at) : ""}</span>
      </div>
    </>
  );
}

function LatencyPanel({ summary }: { summary: PerformanceSummary }) {
  const rows: { label: string; ms: number }[] = [
    { label: "p50", ms: summary.latency.p50Ms },
    { label: "p90", ms: summary.latency.p90Ms },
    { label: "p99", ms: summary.latency.p99Ms },
    { label: "max", ms: summary.latency.maxMs },
  ];
  const scale = Math.max(summary.latency.maxMs, 1);
  return (
    <>
      <div className="flex flex-col gap-[11px]">
        {rows.map((row) => (
          <div key={row.label}>
            <div className="mb-[5px] flex items-center justify-between">
              <span className="text-[12.5px] text-ink-muted">{row.label}</span>
              <span className="font-mono text-[12px]">{row.ms} ms</span>
            </div>
            <div className="h-[6px] overflow-hidden rounded-full bg-well">
              <div
                className="h-full rounded-full bg-accent"
                style={{ width: `${Math.min((row.ms / scale) * 100, 100)}%` }}
              />
            </div>
          </div>
        ))}
      </div>
      <div className="mt-[15px] flex flex-col gap-[9px] border-t border-line-row pt-[14px]">
        <Row label="Slow threshold" value={`${summary.slowThresholdMs} ms`} />
        <Row label="Over threshold" value={summary.latency.overThreshold.toLocaleString()} />
        <Row label="Sample rate" value={`${(summary.sampleRate * 100).toFixed(0)}%`} />
      </div>
      <p className="mt-[12px] text-[11.5px] leading-[1.5] text-ink-faint">
        Threshold and sample rate come from the log settings. Percentiles are estimated from the
        sample, weighted to correct for it — see the row above if the count looks thin.
      </p>
    </>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between text-[12.5px]">
      <span className="text-ink-muted">{label}</span>
      <span className="font-mono text-[12px]">{value}</span>
    </div>
  );
}

const opColumns = "grid-cols-[1.7fr_1.1fr_.7fr_.7fr_.9fr]";

function SlowestOperationsTable({ operations, range }: { operations: SlowestOperation[]; range: PerformanceRange }) {
  const sinceMinutes = range === "1h" ? 60 : range === "24h" ? 1440 : 10080;
  return (
    <>
      <div className={`grid ${opColumns} gap-[10px] border-b border-line-row px-[17px] py-[9px] text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-heading`}>
        <span>Operation</span>
        <span>Bucket</span>
        <span>Calls (est.)</span>
        <span>p95</span>
        <span>Bytes</span>
      </div>
      {operations.map((op, index) => (
        <Link
          key={`${op.operation}-${op.bucket}-${index}`}
          to={`/logs?operation=${encodeURIComponent(op.operation)}&bucket=${encodeURIComponent(op.bucket)}&since=${sinceMinutes}`}
          className={`grid ${opColumns} items-center gap-[10px] border-b border-line-row px-[17px] py-[11px] last:border-b-0 hover:bg-well`}
        >
          <span className="min-w-0 truncate font-mono text-[11.5px] font-semibold text-accent">{op.operation}</span>
          <span className="min-w-0 truncate font-mono text-[11.5px] text-ink-muted">{op.bucket || "—"}</span>
          <span className="font-mono text-[11.5px]">{op.callsEstimate.toLocaleString()}</span>
          <span
            className={`font-mono text-[11.5px] font-semibold ${op.p95Ms >= 500 ? "text-warn" : ""}`}
          >
            {op.p95Ms} ms
          </span>
          <span className="font-mono text-[11.5px] text-ink-muted">{formatBytes(op.bytesEstimate)}</span>
        </Link>
      ))}
      <div className="border-t border-line-row bg-well px-[17px] py-[11px]">
        <p className="text-[11.5px] text-ink-faint">
          Calls and bytes are estimated from the sampled log, weighted to correct for the sample rate
          — not a raw count of rows kept.
        </p>
      </div>
    </>
  );
}

// ─── Live ────────────────────────────────────────────────────────────────────

function LiveView() {
  const [snapshot, setSnapshot] = useState<PerformanceLiveSnapshot | null>(null);
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    const source = new EventSource("/api/performance/live");
    source.onopen = () => setConnected(true);
    source.onerror = () => setConnected(false);
    source.onmessage = (event) => {
      try {
        setSnapshot(JSON.parse(event.data) as PerformanceLiveSnapshot);
      } catch {
        // A malformed tick is dropped rather than shown as an error — the
        // next one arrives in a second regardless.
      }
    };
    return () => source.close();
  }, []);

  const latest = snapshot?.series.at(-1);
  const readBps = latest?.bytesOut ?? 0;
  const writeBps = latest?.bytesIn ?? 0;

  return (
    <>
      {!connected && !snapshot && (
        <div className="mb-[13px] flex items-center gap-[9px] rounded-[11px] border border-line bg-well px-[14px] py-[12px]">
          <Spinner label="" />
          <span className="text-[12.5px] text-ink-muted">Connecting to the live stream…</span>
        </div>
      )}

      <div className="mb-[13px] grid grid-cols-4 gap-[13px]">
        <div className="rounded-[16px] bg-ink p-[16px_17px] text-accent-soft">
          <div className="mb-[8px] flex items-center justify-between">
            <span className="text-[11.5px] text-accent-light">Read</span>
            <span className="size-[6px] flex-none rounded-full bg-accent-light" />
          </div>
          <div className="font-mono text-[25px] font-semibold text-accent-soft">
            {formatBytes(readBps)}
            <span className="text-[13px] font-normal text-accent-light">/s</span>
          </div>
        </div>
        <div className="rounded-[16px] bg-ink p-[16px_17px] text-accent-soft">
          <div className="mb-[8px] flex items-center justify-between">
            <span className="text-[11.5px] text-accent-light">Write</span>
            <span className="size-[6px] flex-none rounded-full bg-accent-light" />
          </div>
          <div className="font-mono text-[25px] font-semibold text-accent-soft">
            {formatBytes(writeBps)}
            <span className="text-[13px] font-normal text-accent-light">/s</span>
          </div>
        </div>
        <KPI
          label="Requests"
          value={snapshot ? `${snapshot.requestsThisSecond}/s` : null}
          hint={snapshot ? `peak ${snapshot.peakRequests}/s in this window` : undefined}
        />
        <KPI
          label="In flight"
          value={snapshot ? String(snapshot.inFlightCount) : null}
          hint={
            snapshot && snapshot.inFlight[0]
              ? `oldest ${Math.round(snapshot.inFlight[0].ageMs / 1000)}s · ${snapshot.inFlight[0].operation || "…"}`
              : undefined
          }
        />
      </div>

      <Card className="mb-[13px] p-[18px]">
        <div className="mb-[15px] flex items-center justify-between">
          <span className="text-[13.5px] font-semibold">Throughput · last 60 seconds</span>
          <span className="font-mono text-[11px] text-ink-faint">1s buckets</span>
        </div>
        {snapshot ? <ThroughputChart series={snapshot.series} /> : <Spinner label="Waiting for data" />}
      </Card>

      <div className="flex items-start gap-[13px]">
        <Card className="min-w-0 flex-[1.5] overflow-hidden">
          <div className="flex items-center gap-[10px] border-b border-line-row px-[17px] py-[15px]">
            <span className="text-[13.5px] font-semibold">In flight now</span>
            <span className="rounded-[6px] bg-well px-[6px] py-[3px] font-mono text-[11px] text-ink-muted">
              {snapshot?.inFlightCount ?? 0}
            </span>
            <span className="ml-auto text-[11.5px] text-ink-faint">open requests, longest first</span>
          </div>
          {!snapshot || snapshot.inFlight.length === 0 ? (
            <EmptyState title="Nothing in flight" hint="Every request finishes before the next tick." />
          ) : (
            <InFlightTable entries={snapshot.inFlight} total={snapshot.inFlightCount} />
          )}
        </Card>

        <SidePanels />
      </div>
    </>
  );
}

function ThroughputChart({ series }: { series: PerformanceLiveSnapshot["series"] }) {
  const peak = Math.max(...series.map((p) => Math.max(p.bytesIn, p.bytesOut)), 1);
  return (
    <div className="flex h-[140px] items-end gap-[2px]">
      {series.map((point) => (
        <div key={point.at} className="flex h-full flex-1 flex-col justify-end gap-[1px]">
          <div
            className="rounded-t-[2px] bg-accent-light"
            style={{ height: `${Math.max((point.bytesOut / peak) * 100, point.bytesOut > 0 ? 3 : 0)}%` }}
            title={`read ${formatBytes(point.bytesOut)}/s`}
          />
          <div
            className="rounded-t-[2px] bg-accent"
            style={{ height: `${Math.max((point.bytesIn / peak) * 100, point.bytesIn > 0 ? 3 : 0)}%` }}
            title={`write ${formatBytes(point.bytesIn)}/s`}
          />
        </div>
      ))}
    </div>
  );
}

const inFlightColumns = "grid-cols-[1.5fr_1.3fr_.7fr]";

function InFlightTable({ entries, total }: { entries: PerformanceInFlightEntry[]; total: number }) {
  return (
    <>
      <div className={`grid ${inFlightColumns} gap-[10px] border-b border-line-row px-[17px] py-[9px] text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-heading`}>
        <span>Operation</span>
        <span>Target</span>
        <span>Age</span>
      </div>
      {entries.map((entry, index) => (
        <div
          key={index}
          className={`grid ${inFlightColumns} items-center gap-[10px] border-b border-line-row px-[17px] py-[11px] last:border-b-0`}
        >
          <span className="min-w-0 truncate font-mono text-[11.5px]">{entry.operation || "…"}</span>
          <span className="min-w-0 truncate font-mono text-[11.5px] text-ink-muted">
            {entry.bucket ? `${entry.bucket}/${entry.key}` : "—"}
          </span>
          <span className="font-mono text-[11.5px] text-warn">{Math.round(entry.ageMs / 1000)}s</span>
        </div>
      ))}
      {total > entries.length && (
        <div className="border-t border-line-row bg-well px-[17px] py-[10px]">
          <p className="text-[11.5px] text-ink-faint">+ {total - entries.length} more, newer</p>
        </div>
      )}
      <div className="border-t border-line-row bg-well px-[17px] py-[10px]">
        <p className="text-[11.5px] text-ink-faint">
          In-flight is process state, not the rollups — it disappears on restart and cannot be
          charted historically.
        </p>
      </div>
    </>
  );
}

// ─── Shared side panels ──────────────────────────────────────────────────────

function SidePanels() {
  return (
    <div className="flex min-w-0 flex-1 flex-col gap-[13px]">
      <FiringAlertsPanel />
      <ServerEventsPanel />
    </div>
  );
}

function FiringAlertsPanel() {
  const [page, setPage] = useState<AlertPage | null>(null);
  useEffect(() => {
    api
      .get<AlertPage>("/api/alerts")
      .then(setPage)
      .catch(() => setPage(null));
  }, []);

  const firing = (page?.alerts ?? []).filter((a: Alert) => a.state !== "resolved");

  return (
    <Card className="p-[17px]">
      <div className="mb-[12px] flex items-center gap-[9px]">
        <span className="text-[13.5px] font-semibold">Firing now</span>
        <span className="ml-auto rounded-[6px] bg-warn-soft px-[6px] py-[3px] font-mono text-[11px] font-semibold text-warn">
          {firing.length}
        </span>
      </div>
      {firing.length === 0 ? (
        <p className="text-[12px] text-ink-faint">Nothing firing.</p>
      ) : (
        <div className="flex flex-col gap-[12px]">
          {firing.slice(0, 4).map((alert) => (
            <div key={alert.id}>
              <div className="flex items-baseline gap-[7px]">
                <Tag tone={alert.severity === "critical" ? "danger" : "warn"}>{alert.severity}</Tag>
                <span className="truncate font-mono text-[11px] text-ink-faint">{alert.rule}</span>
              </div>
              <div className="mt-[4px] text-[12.5px] font-semibold">{alert.summary}</div>
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}

function ServerEventsPanel() {
  const [events, setEvents] = useState<ServerEvent[] | null>(null);
  useEffect(() => {
    api
      .get<{ events: ServerEvent[] }>("/api/logs/events?limit=5")
      .then((page) => setEvents(page.events))
      .catch(() => setEvents(null));
  }, []);

  return (
    <Card className="p-[17px]">
      <div className="mb-[12px] text-[13.5px] font-semibold">Server events</div>
      {!events || events.length === 0 ? (
        <p className="text-[12px] text-ink-faint">Nothing recent.</p>
      ) : (
        <div className="flex flex-col gap-[11px]">
          {events.map((event) => (
            <div key={event.id} className="flex items-start gap-[9px]">
              <Tag tone={event.level === "ERROR" ? "danger" : "warn"}>{event.level.toLowerCase()}</Tag>
              <div className="min-w-0">
                <div className="text-[12.5px] leading-[1.45]">{event.message}</div>
                <p className="mt-[2px] text-[11.5px] text-ink-faint">{formatDate(event.at)}</p>
              </div>
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}

// ─── Small pieces ────────────────────────────────────────────────────────────

function KPI({
  label,
  value,
  hint,
  tone,
}: {
  label: string;
  value: string | null;
  hint?: string;
  tone?: "danger";
}) {
  return (
    <Card className="p-[16px_17px]">
      <div className="mb-[9px] text-[11.5px] text-ink-muted">{label}</div>
      {value === null ? (
        <div className="h-[24px] w-[70%] animate-pulse rounded-[6px] bg-well" />
      ) : (
        <div className={`text-[23px] font-semibold tracking-[-0.02em] ${tone === "danger" ? "text-danger" : ""}`}>
          {value}
        </div>
      )}
      {hint && <p className="mt-[6px] text-[11.5px] text-ink-faint">{hint}</p>}
    </Card>
  );
}

function rangeLabel(range: PerformanceRange): string {
  return range === "1h" ? "Last hour" : range === "24h" ? "Last 24 hours" : "Last 7 days";
}

function compact(value: number): string {
  return new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 }).format(value);
}

// CSV is generated client-side from data already fetched — no export endpoint
// needed, and nothing here is sensitive that a page the user is already
// signed into cannot show them directly.
function downloadCSV(summary: PerformanceSummary, range: PerformanceRange) {
  const lines = [
    "metric,value",
    `range,${range}`,
    `requests,${summary.requests}`,
    `client_errors,${summary.clientErrors}`,
    `server_errors,${summary.serverErrors}`,
    `error_rate,${summary.errorRate}`,
    `p50_ms,${summary.latency.p50Ms}`,
    `p90_ms,${summary.latency.p90Ms}`,
    `p99_ms,${summary.latency.p99Ms}`,
    `max_ms,${summary.latency.maxMs}`,
    `over_threshold,${summary.latency.overThreshold}`,
    "",
    "operation,bucket,calls_estimate,p95_ms,bytes_estimate",
    ...summary.slowestOperations.map(
      (op) => `${csvField(op.operation)},${csvField(op.bucket)},${op.callsEstimate},${op.p95Ms},${op.bytesEstimate}`,
    ),
  ];

  const blob = new Blob([lines.join("\n")], { type: "text/csv" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `pail-performance-${range}-${new Date().toISOString().slice(0, 10)}.csv`;
  document.body.appendChild(link);
  link.click();
  document.body.removeChild(link);
  URL.revokeObjectURL(url);
}

function csvField(value: string): string {
  if (value.includes(",") || value.includes('"')) {
    return `"${value.replace(/"/g, '""')}"`;
  }
  return value;
}
