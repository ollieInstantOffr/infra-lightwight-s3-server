import { Link } from "react-router-dom";
import { useApi } from "../lib/useApi";
import type { Dashboard } from "../lib/api";
import { formatBytes } from "../lib/format";
import { Card, ErrorNotice, PageHeader, Spinner } from "../components/ui";

export function DashboardPage() {
  const { data, error, loading, reload } = useApi<Dashboard>("/api/dashboard");

  if (loading) return <Spinner label="Loading overview" />;
  if (error) return <ErrorNotice message={error} onRetry={reload} />;
  if (!data) return null;

  const usedFraction = data.diskTotal > 0 ? 1 - data.diskFree / data.diskTotal : 0;

  return (
    <>
      <PageHeader title="Overview" description="What this server is currently holding." />

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Stat label="Buckets" value={data.buckets.toLocaleString()} />
        <Stat label="Objects" value={data.objects.toLocaleString()} />
        <Stat label="Stored" value={formatBytes(data.bytesStored)} />
        <Stat label="Disk free" value={formatBytes(data.diskFree)} />
      </div>

      <Card className="mt-4 p-5">
        <div className="flex items-baseline justify-between gap-4">
          <h2 className="text-sm font-medium">Volume</h2>
          <span className="text-sm text-ink-muted">
            {formatBytes(data.diskTotal - data.diskFree)} of {formatBytes(data.diskTotal)} used
          </span>
        </div>
        <div
          className="mt-3 h-2 overflow-hidden rounded-full bg-border"
          role="progressbar"
          aria-valuenow={Math.round(usedFraction * 100)}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label="Disk used"
        >
          <div
            className={`h-full rounded-full ${usedFraction > 0.9 ? "bg-danger" : "bg-accent"}`}
            style={{ width: `${Math.min(usedFraction * 100, 100)}%` }}
          />
        </div>
        {usedFraction > 0.9 && (
          <p className="mt-2 text-sm text-danger">
            The volume is nearly full. Uploads will start failing.
          </p>
        )}
      </Card>

      {/* Stated here rather than buried in documentation. Anyone reading a
          storage dashboard should know what it does and does not promise. */}
      <Card className="mt-4 border-dashed p-5">
        <h2 className="text-sm font-medium">Data protection</h2>
        <p className="mt-1 text-sm text-ink-muted">{data.durabilityNote}</p>
        <p className="mt-2 text-sm text-ink-muted">
          Back up the data volume and a <code className="text-xs">pg_dump</code> together — neither is
          usable without the other.
        </p>
      </Card>

      <p className="mt-6 text-sm text-ink-muted">
        <Link to="/buckets" className="underline underline-offset-2">
          Browse buckets
        </Link>
      </p>
    </>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <Card className="p-5">
      <p className="text-sm text-ink-muted">{label}</p>
      <p className="mt-1 text-2xl font-semibold tabular-nums tracking-tight">{value}</p>
    </Card>
  );
}
