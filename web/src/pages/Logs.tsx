import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api, type ErrorGroup, type LogEntry, type LogPage, type LogSummary, type ServerEvent } from "../lib/api";
import { formatBytes, formatDate, formatRelative } from "../lib/format";
import {
  Button,
  Card,
  EmptyState,
  ErrorNotice,
  InfoNotice,
  PageHeader,
  Select,
  SkeletonLine,
  Tabs,
  Tag,
  TableHead,
  TableRow,
  TextInput,
  Drawer,
  CopyButton,
} from "../components/ui";

// The screen this epic exists for. The server has always known why requests
// failed; until now that explanation only reached stdout.

type Tab = "requests" | "events";

const columns = "grid-cols-[120px_58px_1fr_auto_86px_92px]";

export function LogsPage() {
  const [params, setParams] = useSearchParams();
  const [tab, setTab] = useState<Tab>("requests");

  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [nextBefore, setNextBefore] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [inspecting, setInspecting] = useState<LogEntry | null>(null);
  const [summary, setSummary] = useState<LogSummary | null>(null);
  const [live, setLive] = useState(false);

  // Filters live in the URL so a diagnosis can be shared as a link.
  const errorsOnly = params.get("errors") === "1";
  const slowOnly = params.get("slow") === "1";
  const surface = params.get("surface") ?? "";
  const code = params.get("code") ?? "";
  const bucket = params.get("bucket") ?? "";
  const operation = params.get("operation") ?? "";
  const accessKeyId = params.get("accessKeyId") ?? "";
  const since = params.get("since") ?? "60";
  const [search, setSearch] = useState(params.get("q") ?? "");

  const setParam = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next, { replace: true });
  };

  const query = useCallback(
    (extra?: Record<string, string>) => {
      const q = new URLSearchParams();
      if (errorsOnly) q.set("errors", "1");
      if (slowOnly) q.set("slow", "1");
      if (surface) q.set("surface", surface);
      if (code) q.set("code", code);
      if (bucket) q.set("bucket", bucket);
      if (operation) q.set("operation", operation);
      if (accessKeyId) q.set("accessKeyId", accessKeyId);
      if (since) q.set("since", since);
      const term = params.get("q");
      if (term) q.set("q", term);
      for (const [k, v] of Object.entries(extra ?? {})) q.set(k, v);
      return q;
    },
    [errorsOnly, slowOnly, surface, code, bucket, operation, accessKeyId, since, params],
  );

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const page = await api.get<LogPage>(`/api/logs?${query().toString()}`);
      setEntries(page.logs);
      setNextBefore(page.nextBefore);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not load the log.");
    } finally {
      setLoading(false);
    }
  }, [query]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    api
      .get<LogSummary>(`/api/logs/summary?minutes=${since || 60}`)
      .then(setSummary)
      .catch(() => setSummary(null));
  }, [since, entries.length]);

  // Live tail. Server-sent events rather than polling: one connection, pushed
  // from the server, and it stops cleanly when the toggle goes off.
  const seen = useRef<Set<number>>(new Set());
  useEffect(() => {
    if (!live || tab !== "requests") return;

    const source = new EventSource(`/api/logs/stream?${query().toString()}`);
    source.onmessage = (event) => {
      try {
        const entry = JSON.parse(event.data) as LogEntry;
        if (seen.current.has(entry.id)) return;
        seen.current.add(entry.id);
        // Bounded, dropping oldest, so a tab left open overnight does not
        // consume a gigabyte.
        setEntries((current) => [entry, ...current].slice(0, 500));
      } catch {
        // A malformed frame is not worth breaking the stream over.
      }
    };
    return () => source.close();
  }, [live, tab, query]);

  async function loadMore() {
    if (nextBefore === null) return;
    try {
      const page = await api.get<LogPage>(`/api/logs?${query({ before: String(nextBefore) }).toString()}`);
      setEntries((current) => [...current, ...page.logs]);
      setNextBefore(page.nextBefore);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not load more.");
    }
  }

  return (
    <>
      <PageHeader
        title="Logs"
        subtitle="Every failed request, with the reason the server actually had."
        actions={
          <>
            <Button
              variant={live ? "primary" : "secondary"}
              onClick={() => {
                seen.current.clear();
                setLive((on) => !on);
              }}
            >
              {live ? "Live · stop" : "Live tail"}
            </Button>
            <Button
              variant={slowOnly ? "primary" : "secondary"}
              onClick={() => setParam("slow", slowOnly ? "" : "1")}
              title="Requests at or above the configured slow threshold — the same one the Performance page's latency panel uses."
            >
              Slow only
            </Button>
            <Button onClick={load}>Refresh</Button>
          </>
        }
      />

      {error && <ErrorNotice message={error} onRetry={load} />}

      {summary && summary.groups.length > 0 && (
        <ErrorSummary
          summary={summary}
          onFilter={(group) => {
            setParam("code", group.errorCode);
            if (group.accessKeyId) setParam("accessKeyId", group.accessKeyId);
            setParam("errors", "1");
          }}
        />
      )}

      <Tabs
        tabs={[
          { id: "requests", label: "Requests" },
          { id: "events", label: "Server events" },
        ]}
        active={tab}
        onChange={setTab}
      />

      {tab === "events" ? (
        <ServerEvents />
      ) : (
        <>
          <Card className="mb-[14px] p-[14px]">
            <div className="flex flex-wrap items-center gap-[8px]">
              <div className="w-[240px]">
                <TextInput
                  value={search}
                  onChange={setSearch}
                  placeholder="Key, reason, or request id"
                  ariaLabel="Search the log"
                  onKeyDown={(event) => {
                    if (event.key === "Enter") setParam("q", search);
                  }}
                />
              </div>
              <Select
                value={since}
                onChange={(value) => setParam("since", value)}
                ariaLabel="Time range"
                options={[
                  { value: "15", label: "Last 15 minutes" },
                  { value: "60", label: "Last hour" },
                  { value: "360", label: "Last 6 hours" },
                  { value: "1440", label: "Last 24 hours" },
                  { value: "10080", label: "Last 7 days" },
                ]}
              />
              <Select
                value={errorsOnly ? "1" : ""}
                onChange={(value) => setParam("errors", value)}
                ariaLabel="Outcome"
                options={[
                  { value: "", label: "All requests" },
                  { value: "1", label: "Failures only" },
                ]}
              />
              <Select
                value={surface}
                onChange={(value) => setParam("surface", value)}
                ariaLabel="Surface"
                options={[
                  { value: "", label: "S3 and console" },
                  { value: "s3", label: "S3 API" },
                  { value: "console", label: "Console" },
                ]}
              />
              {(code || bucket || operation || accessKeyId || params.get("q")) && (
                <Button
                  onClick={() => {
                    setSearch("");
                    setParams(new URLSearchParams({ since }), { replace: true });
                  }}
                >
                  Clear filters
                </Button>
              )}
            </div>

            {(code || bucket || accessKeyId) && (
              <div className="mt-[10px] flex flex-wrap items-center gap-[6px] text-[11.5px] text-ink-muted">
                <span>Filtered to</span>
                {code && <Tag tone="danger">{code}</Tag>}
                {bucket && <Tag tone="mono">{bucket}</Tag>}
                {accessKeyId && <Tag tone="mono">{accessKeyId}</Tag>}
              </div>
            )}
          </Card>

          <Card className="overflow-hidden">
            <TableHead
              columns={["When", "Status", "Request", "Reason", "Duration", "Size"]}
              className={columns}
            />

            {loading &&
              entries.length === 0 &&
              Array.from({ length: 8 }, (_, index) => (
                <div key={index} className={`grid ${columns} gap-[10px] border-b border-line-row px-[18px] py-[11px]`}>
                  <SkeletonLine width={90} />
                  <SkeletonLine width={34} faint />
                  <SkeletonLine width="70%" faint />
                  <SkeletonLine width="50%" faint />
                  <SkeletonLine width={40} faint />
                  <SkeletonLine width={46} faint />
                </div>
              ))}

            {entries.map((entry) => (
              <TableRow key={entry.id} className={columns} onClick={() => setInspecting(entry)}>
                <span className="text-[12px] text-ink-muted">{formatRelative(entry.at)}</span>
                <span>
                  <StatusTag status={entry.status} />
                </span>
                <span className="min-w-0 truncate font-mono text-[12px]">
                  <span className="text-ink-faint">{entry.method}</span> {entry.path}
                </span>
                <span className="min-w-0 truncate text-[12px] text-ink-muted" title={entry.reason}>
                  {entry.errorCode && <Tag tone="danger">{entry.errorCode}</Tag>}{" "}
                  {entry.reason}
                </span>
                <span className="text-right text-[12px] tabular-nums text-ink-muted">
                  {entry.durationMs} ms
                </span>
                <span className="text-right text-[12px] tabular-nums text-ink-muted">
                  {formatBytes(entry.bytesOut || entry.bytesIn)}
                </span>
              </TableRow>
            ))}

            {!loading && entries.length === 0 && (
              <EmptyState
                title="Nothing in this window"
                hint={
                  errorsOnly
                    ? "No failures in the selected period, which is the good outcome."
                    : "Successful requests are sampled, so a quiet server shows few rows. Failures are always kept."
                }
              />
            )}

            {nextBefore !== null && !live && (
              <div className="border-t border-line-row px-[18px] py-[12px] text-center">
                <Button onClick={loadMore}>Load more</Button>
              </div>
            )}
          </Card>
        </>
      )}

      {inspecting && <LogDetail entry={inspecting} onClose={() => setInspecting(null)} />}
    </>
  );
}

function StatusTag({ status }: { status: number }) {
  const tone = status >= 500 ? "danger" : status >= 400 ? "warn" : "accent";
  return <Tag tone={tone}>{status}</Tag>;
}

/**
 * Grouped failures.
 *
 * A list of a thousand 403s is not a diagnosis; a count of them by cause is.
 */
function ErrorSummary({
  summary,
  onFilter,
}: {
  summary: LogSummary;
  onFilter: (group: ErrorGroup) => void;
}) {
  const total = summary.groups.reduce((sum, group) => sum + group.count, 0);

  return (
    <Card className="mb-[16px] p-[17px]">
      <div className="mb-[12px] flex items-baseline justify-between">
        <h2 className="m-0 text-[14px] font-semibold">
          {total.toLocaleString()} failures in the last{" "}
          {summary.windowMinutes >= 60
            ? `${Math.round(summary.windowMinutes / 60)} hours`
            : `${summary.windowMinutes} minutes`}
        </h2>
        <span className="text-[11.5px] text-ink-faint">Grouped by cause</span>
      </div>

      <div className="space-y-[8px]">
        {summary.groups.slice(0, 5).map((group, index) => (
          <button
            key={index}
            onClick={() => onFilter(group)}
            className="block w-full rounded-[12px] border border-line bg-inset p-[13px] text-left hover:border-accent"
          >
            <div className="flex flex-wrap items-center gap-[8px]">
              <span className="text-[15px] font-semibold tabular-nums">{group.count.toLocaleString()}</span>
              <Tag tone="danger">{group.errorCode || "error"}</Tag>
              {group.bucket && <Tag tone="mono">{group.bucket}</Tag>}
              {group.accessKeyId && <Tag tone="mono">{group.accessKeyId}</Tag>}
              <span className="ml-auto text-[11px] text-ink-faint">{formatRelative(group.lastSeen)}</span>
            </div>
            {group.likelyCause && (
              <p className="m-0 mt-[7px] text-[12px] leading-[1.6] text-ink-muted">{group.likelyCause}</p>
            )}
            {!group.likelyCause && group.reason && (
              <p className="m-0 mt-[7px] font-mono text-[11.5px] text-ink-muted">{group.reason}</p>
            )}
          </button>
        ))}
      </div>
    </Card>
  );
}

function ServerEvents() {
  const [events, setEvents] = useState<ServerEvent[] | null>(null);
  const [level, setLevel] = useState("");

  useEffect(() => {
    api
      .get<{ events: ServerEvent[] }>(`/api/logs/events${level ? `?level=${level}` : ""}`)
      .then((result) => setEvents(result.events))
      .catch(() => setEvents([]));
  }, [level]);

  return (
    <>
      <div className="mb-[14px]">
        <Select
          value={level}
          onChange={setLevel}
          ariaLabel="Level"
          options={[
            { value: "", label: "Warnings and errors" },
            { value: "WARN", label: "Warnings" },
            { value: "ERROR", label: "Errors" },
          ]}
        />
      </div>
      <Card className="overflow-hidden">
        <TableHead columns={["When", "Level", "Message"]} className="grid-cols-[150px_80px_1fr]" />
        {events?.map((event) => (
          <TableRow key={event.id} className="grid-cols-[150px_80px_1fr]">
            <span className="text-[12px] text-ink-muted">{formatDate(event.at)}</span>
            <span>
              <Tag tone={event.level === "ERROR" ? "danger" : "warn"}>{event.level.toLowerCase()}</Tag>
            </span>
            <span className="min-w-0">
              <span className="text-[12.5px]">{event.message}</span>
              {event.attributes && Object.keys(event.attributes).length > 0 && (
                <span className="mt-[3px] block truncate font-mono text-[11px] text-ink-faint">
                  {Object.entries(event.attributes)
                    .map(([key, value]) => `${key}=${String(value)}`)
                    .join("  ")}
                </span>
              )}
            </span>
          </TableRow>
        ))}
        {events?.length === 0 && (
          <EmptyState
            title="Nothing recorded"
            hint="Warnings and errors the server raises about itself appear here — a failed email send, a blob it could not reclaim. Requests have their own tab."
          />
        )}
      </Card>
    </>
  );
}

function LogDetail({ entry, onClose }: { entry: LogEntry; onClose: () => void }) {
  const rows: [string, string][] = [
    ["Request id", entry.requestId || "—"],
    ["When", formatDate(entry.at)],
    ["Surface", entry.surface],
    ["Method", entry.method],
    ["Path", entry.path],
    ["Bucket", entry.bucket || "—"],
    ["Key", entry.key || "—"],
    ["Status", String(entry.status)],
    ["Error code", entry.errorCode || "—"],
    ["Duration", `${entry.durationMs} ms`],
    ["Bytes in", formatBytes(entry.bytesIn)],
    ["Bytes out", formatBytes(entry.bytesOut)],
    ["Access key", entry.accessKeyId || "—"],
    ["Actor", entry.actor || "—"],
    ["Client", entry.clientIp || "—"],
    ["User agent", entry.userAgent || "—"],
  ];

  return (
    <Drawer
      title={`${entry.method} ${entry.path}`}
      subtitle={<StatusTag status={entry.status} />}
      onClose={onClose}
    >
      <div className="space-y-[16px]">
        {entry.reason && (
          <InfoNotice tone={entry.status >= 500 ? "warn" : "accent"}>
            <p className="m-0 mb-[4px] font-semibold">Why it failed</p>
            <p className="m-0 font-mono text-[11.5px] leading-[1.6]">{entry.reason}</p>
            <p className="m-0 mt-[8px] text-[11.5px]">
              This explanation is kept for you and is never sent to the client, which saw only{" "}
              <span className="font-mono">{entry.errorCode || entry.status}</span>.
            </p>
          </InfoNotice>
        )}

        <dl className="grid grid-cols-[96px_1fr] gap-x-[12px] gap-y-[7px] text-[12.5px]">
          {rows.map(([label, value]) => (
            <div key={label} className="contents">
              <dt className="text-ink-muted">{label}</dt>
              <dd className="m-0 break-all font-mono text-[11.5px]">{value}</dd>
            </div>
          ))}
        </dl>

        {entry.requestId && (
          <CopyBlockish requestId={entry.requestId} />
        )}
      </div>
    </Drawer>
  );
}

function CopyBlockish({ requestId }: { requestId: string }) {
  return (
    <div className="flex items-center gap-[8px]">
      <CopyButton text={requestId}>Copy request id</CopyButton>
      <span className="text-[11.5px] text-ink-faint">
        The same id appears in the server's own log and in the response header.
      </span>
    </div>
  );
}
