import { useApi } from "../lib/useApi";
import type { SystemStatus } from "../lib/api";
import { formatBytes, formatDate } from "../lib/format";
import {
  Card,
  ErrorNotice,
  InfoNotice,
  PageHeader,
  ProgressBar,
  Spinner,
  StatusDot,
} from "../components/ui";

export function SystemPage() {
  const { data, error, loading, reload } = useApi<SystemStatus>("/api/system");

  if (loading) return <Spinner label="Reading node status" />;
  if (error) return <ErrorNotice message={error} onRetry={reload} />;
  if (!data) return null;

  const usedFraction =
    data.storage.diskTotal > 0
      ? (data.storage.diskTotal - data.storage.diskFree) / data.storage.diskTotal
      : 0;

  return (
    <>
      <PageHeader
        title="System & health"
        subtitle={
          <>
            Node <span className="font-mono text-[12.5px] font-medium text-ink">{data.node.name}</span> ·
            up {formatUptime(data.node.uptime)}
          </>
        }
      />

      {/* Warnings first: this screen exists to be looked at when something is
          wrong, and each one names a specific thing to act on. */}
      {data.warnings.length > 0 && (
        <div className="mb-[16px] space-y-[8px]">
          {data.warnings.map((warning) => (
            <ErrorNotice key={warning.area} title={warning.area} message={warning.message} />
          ))}
        </div>
      )}

      <div className="grid gap-[16px] lg:grid-cols-2">
        <Card padded>
          <h2 className="m-0 mb-[12px] text-[14px] font-semibold">Node</h2>
          <Rows
            rows={[
              ["Version", data.node.version],
              ["Go", data.node.go],
              ["Environment", data.node.environment],
              ["Started", formatDate(data.node.startedAt)],
              ["Uptime", formatUptime(data.node.uptime)],
            ]}
          />
        </Card>

        <Card padded>
          <h2 className="m-0 mb-[12px] text-[14px] font-semibold">Database</h2>
          <div className="mb-[12px]">
            <StatusDot tone={data.database.reachable ? "ok" : "danger"}>
              {data.database.reachable ? "Reachable" : "Unreachable"}
            </StatusDot>
          </div>
          <Rows
            rows={[
              ["Connections", `${data.database.connections} of ${data.database.maxConnections}`],
              ["In use", String(data.database.acquiredConns)],
              ["Idle", String(data.database.idleConnections)],
            ]}
          />
        </Card>

        <Card padded>
          <h2 className="m-0 mb-[12px] text-[14px] font-semibold">Storage</h2>
          <div className="mb-[10px] flex justify-between text-[12.5px]">
            <span className="text-ink-muted">
              {formatBytes(data.storage.diskTotal - data.storage.diskFree)} used
            </span>
            <span className="font-mono text-[12px]">{formatBytes(data.storage.diskTotal)}</span>
          </div>
          <ProgressBar
            fraction={usedFraction}
            tone={usedFraction > 0.9 ? "danger" : usedFraction > 0.85 ? "warn" : "accent"}
            label="Disk used"
          />
          <div className="mt-[14px]">
            <Rows
              rows={[
                ["Data directory", data.storage.dataDir],
                ["Free", formatBytes(data.storage.diskFree)],
                ["Readable", data.storage.readable ? "yes" : "no"],
              ]}
            />
          </div>
          <div className="mt-[14px]">
            <InfoNotice>
              Objects are stored as a single copy. There is no replication, parity or repair — a lost
              disk is lost data. Back up the volume and a <span className="font-mono">pg_dump</span>{" "}
              together.
            </InfoNotice>
          </div>
        </Card>

        <Card padded>
          <h2 className="m-0 mb-[12px] text-[14px] font-semibold">Configuration</h2>
          <Rows
            rows={[
              ["S3 endpoint", data.endpoints.s3],
              ["Console", data.endpoints.console],
              ["Region", data.endpoints.region],
              [
                "Virtual-host style",
                data.endpoints.virtualHostStyle ? data.endpoints.s3Domain : "off (path style only)",
              ],
              ["Outbound email", data.config.resendConfigured ? "configured" : "not configured"],
              ["Trusted proxies", `${data.config.trustedProxyCount} ranges`],
            ]}
          />
          <div className="mt-[14px]">
            <Rows
              rows={[
                ["Users", String(data.counts.users)],
                ["Access keys", `${data.counts.activeCredentials} active of ${data.counts.credentials}`],
              ]}
            />
          </div>
        </Card>
      </div>
    </>
  );
}

function Rows({ rows }: { rows: [string, string][] }) {
  return (
    <dl className="grid grid-cols-[130px_1fr] gap-x-[12px] gap-y-[7px] text-[12.5px]">
      {rows.map(([label, value]) => (
        <div key={label} className="contents">
          <dt className="text-ink-muted">{label}</dt>
          <dd className="m-0 break-all font-mono text-[12px]">{value}</dd>
        </div>
      ))}
    </dl>
  );
}

function formatUptime(seconds: number): string {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}
