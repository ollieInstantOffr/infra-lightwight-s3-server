import { Link } from "react-router-dom";
import { useApi } from "../lib/useApi";
import type { Bucket, Dashboard, Traffic } from "../lib/api";
import { formatBytes, formatDate } from "../lib/format";
import {
  Card,
  ErrorNotice,
  InfoNotice,
  PageHeader,
  SkeletonLine,
  TableHead,
  TableRow,
  Button,
} from "../components/ui";

export function DashboardPage() {
  const dashboard = useApi<Dashboard>("/api/dashboard");
  const traffic = useApi<Traffic>("/api/traffic?days=14");
  const buckets = useApi<{ buckets: Bucket[] }>("/api/buckets");

  const data = dashboard.data;
  const used = data ? data.diskTotal - data.diskFree : 0;

  return (
    <>
      <PageHeader
        title="Overview"
        subtitle={
          data ? (
            <>
              {data.buckets} {data.buckets === 1 ? "bucket" : "buckets"} ·{" "}
              {data.objects.toLocaleString()} objects · single copy, no replication
            </>
          ) : (
            "Reading node stats…"
          )
        }
        actions={
          <>
            <Link to="/endpoint">
              <Button>View endpoint</Button>
            </Link>
            <Link to="/buckets">
              <Button variant="primary">New bucket</Button>
            </Link>
          </>
        }
      />

      {dashboard.error && <ErrorNotice message={dashboard.error} onRetry={dashboard.reload} />}

      <div className="mb-[20px] grid grid-cols-4 gap-[13px] max-lg:grid-cols-2">
        <Stat
          label="Stored"
          value={data ? formatBytes(data.bytesStored) : null}
          hint={data ? `across ${data.buckets} ${data.buckets === 1 ? "bucket" : "buckets"}` : ""}
        />
        <Stat
          label="Objects"
          value={data ? data.objects.toLocaleString() : null}
          hint={data ? `${formatBytes(data.bytesStored)} total` : ""}
        />
        <Stat
          label="Disk free"
          value={data ? formatBytes(data.diskFree) : null}
          hint={data ? `of ${formatBytes(data.diskTotal)}` : ""}
        />
        <Stat
          label="Requests (24h)"
          value={traffic.data ? compactNumber(traffic.data.requests24h) : null}
          hint={
            traffic.data
              ? `${(traffic.data.errorRate * 100).toFixed(2)}% errors`
              : "counting…"
          }
        />
      </div>

      <div className="flex gap-[16px] max-lg:flex-col">
        <div className="flex min-w-0 flex-[2.1] flex-col gap-[16px]">
          <Card className="px-[18px] pb-[12px] pt-[18px]">
            <div className="mb-[16px] flex items-baseline justify-between">
              <span className="text-[13.5px] font-semibold">Requests, last 14 days</span>
              <span className="font-mono text-[11px] text-ink-faint">
                {traffic.data ? `${compactNumber(totalRequests(traffic.data))} total` : "—"}
              </span>
            </div>
            <TrafficChart traffic={traffic.data} />
          </Card>

          <Card className="overflow-hidden">
            <div className="flex items-center justify-between border-b border-line-row px-[18px] py-[15px]">
              <span className="text-[13.5px] font-semibold">Buckets</span>
              <Link to="/buckets" className="text-[12.5px] font-semibold text-accent hover:text-accent-hover">
                See all
              </Link>
            </div>
            <TableHead
              columns={["Name", "Created", "Objects", "Size"]}
              className="grid-cols-[2.4fr_1fr_.8fr_.8fr]"
            />
            {buckets.data?.buckets.slice(0, 6).map((bucket) => (
              <TableRow key={bucket.name} className="grid-cols-[2.4fr_1fr_.8fr_.8fr]">
                <Link
                  to={`/buckets/${encodeURIComponent(bucket.name)}`}
                  className="truncate font-mono text-[12.5px] font-medium underline-offset-2 hover:underline"
                >
                  {bucket.name}
                </Link>
                <span className="text-[12.5px] text-ink-muted">{formatDate(bucket.createdAt)}</span>
                <span className="tabular-nums text-ink-muted">{bucket.objectCount.toLocaleString()}</span>
                <span className="tabular-nums text-ink-muted">{formatBytes(bucket.totalBytes)}</span>
              </TableRow>
            ))}
            {buckets.data?.buckets.length === 0 && (
              <p className="px-[18px] py-[26px] text-center text-[12.5px] text-ink-muted">
                No buckets yet. <Link to="/buckets" className="text-accent">Create one.</Link>
              </p>
            )}
          </Card>
        </div>

        <div className="flex w-[300px] flex-none flex-col gap-[16px] max-lg:w-full">
          <Card padded>
            <p className="m-0 mb-[12px] text-[13.5px] font-semibold">Volume</p>
            <div className="mb-[8px] flex justify-between text-[11.5px] text-ink-muted">
              <span>{data ? formatBytes(used) : "—"} used</span>
              <span>{data ? formatBytes(data.diskTotal) : "—"}</span>
            </div>
            <div className="h-[5px] overflow-hidden rounded-full bg-[#e2ebe8]">
              <div
                className={`h-full ${
                  data && data.diskFree / data.diskTotal < 0.1 ? "bg-danger" : "bg-accent"
                }`}
                style={{
                  width: data && data.diskTotal > 0 ? `${(used / data.diskTotal) * 100}%` : "0%",
                }}
              />
            </div>
            {data && data.diskFree / data.diskTotal < 0.1 && (
              <p className="mt-[10px] text-[12px] text-danger">
                Under 10% free. Uploads will start failing.
              </p>
            )}
          </Card>

          {/* Stated where an operator reading a storage dashboard will see it,
              rather than only in the documentation. */}
          <InfoNotice>
            <p className="m-0 mb-[4px] font-semibold">Data protection</p>
            <p className="m-0">
              {data?.durabilityNote ?? "Objects are stored as a single copy."} Back up the data volume
              and a <span className="font-mono">pg_dump</span> together — neither is usable without the
              other.
            </p>
          </InfoNotice>
        </div>
      </div>
    </>
  );
}

function Stat({ label, value, hint }: { label: string; value: string | null; hint: string }) {
  return (
    <Card className="px-[17px] py-[16px]">
      <div className="mb-[9px] text-[11.5px] text-ink-muted">{label}</div>
      {value === null ? (
        <SkeletonLine width={86} height={20} />
      ) : (
        <div className="text-[23px] font-semibold tracking-[-0.02em] tabular-nums">{value}</div>
      )}
      <div className="mt-[6px] text-[11.5px] text-ink-faint">{hint}</div>
    </Card>
  );
}

function TrafficChart({ traffic }: { traffic: Traffic | null }) {
  if (!traffic) {
    return (
      <div className="flex h-[110px] items-end gap-[6px]">
        {Array.from({ length: 14 }, (_, index) => (
          <SkeletonLine key={index} width="100%" height={30 + ((index * 13) % 60)} />
        ))}
      </div>
    );
  }

  const peak = Math.max(...traffic.daily.map((day) => day.requests), 1);
  return (
    <>
      <div className="flex h-[110px] items-end gap-[6px]">
        {traffic.daily.map((day, index) => {
          // The most recent days are drawn in the full accent, older ones
          // washed out, so "now" reads without needing an axis label.
          const age = traffic.daily.length - index;
          const colour = age <= 3 ? "bg-accent" : age <= 7 ? "bg-[#c6e2d8]" : "bg-accent-wash";
          const height = Math.max((day.requests / peak) * 100, day.requests > 0 ? 4 : 1.5);
          return (
            <div
              key={day.day}
              className={`flex-1 rounded-t-[4px] ${colour}`}
              style={{ height: `${height}%` }}
              title={`${new Date(day.day).toLocaleDateString()}: ${day.requests.toLocaleString()} requests, ${day.errors.toLocaleString()} errors`}
            />
          );
        })}
      </div>
      <div className="mt-[10px] flex justify-between text-[11.5px] text-ink-faint">
        <span>{traffic.daily[0] ? new Date(traffic.daily[0].day).toLocaleDateString(undefined, { month: "short", day: "numeric" }) : ""}</span>
        <span>
          {traffic.daily.at(-1)
            ? new Date(traffic.daily.at(-1)!.day).toLocaleDateString(undefined, { month: "short", day: "numeric" })
            : ""}
        </span>
      </div>
    </>
  );
}

function totalRequests(traffic: Traffic): number {
  return traffic.daily.reduce((sum, day) => sum + day.requests, 0);
}

/** Compact counts, so a busy node does not push the card wider. */
function compactNumber(value: number): string {
  return new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 }).format(value);
}
