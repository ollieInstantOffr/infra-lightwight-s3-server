import { useCallback, useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, type Bucket, type BucketSettings } from "../lib/api";
import { breadcrumbs, formatBytes } from "../lib/format";
import { Crumbs, CrumbSeparator, PageHeader, Tabs, Tag } from "../components/ui";
import { ObjectBrowser } from "./bucket/ObjectBrowser";
import { BucketSettingsTab } from "./bucket/BucketSettings";
import { BucketVersionsTab } from "./bucket/BucketVersions";

type Tab = "objects" | "versions" | "settings";

export function ObjectsPage() {
  const { bucket = "", "*": prefix = "" } = useParams();
  const [tab, setTab] = useState<Tab>("objects");
  const [settings, setSettings] = useState<BucketSettings | null>(null);
  const [summary, setSummary] = useState<Bucket | null>(null);

  // Settings drive more than the settings tab: whether versioning is on
  // changes what deleting an object means, so the browser needs to know.
  const loadContext = useCallback(() => {
    void api
      .get<BucketSettings>(`/api/buckets/${encodeURIComponent(bucket)}/settings`)
      .then(setSettings)
      .catch(() => setSettings(null));
    void api
      .get<{ buckets: Bucket[] }>("/api/buckets")
      .then((result) => setSummary(result.buckets.find((entry) => entry.name === bucket) ?? null))
      .catch(() => setSummary(null));
  }, [bucket]);

  useEffect(loadContext, [loadContext]);

  const crumbs = breadcrumbs(prefix);

  return (
    <>
      <Crumbs>
        <Link to="/buckets" className="text-ink-muted underline-offset-2 hover:underline">
          Buckets
        </Link>
        <CrumbSeparator />
        <Link
          to={`/buckets/${encodeURIComponent(bucket)}`}
          className="font-mono font-medium underline-offset-2 hover:underline"
        >
          {bucket}
        </Link>
        {crumbs.map((crumb) => (
          <span key={crumb.prefix} className="flex items-center gap-[7px]">
            <CrumbSeparator />
            <Link
              to={`/buckets/${encodeURIComponent(bucket)}/${crumb.prefix}`}
              className="font-mono underline-offset-2 hover:underline"
            >
              {crumb.name}
            </Link>
          </span>
        ))}
      </Crumbs>

      <PageHeader
        title={bucket}
        mono
        subtitle={
          <span className="flex flex-wrap items-center gap-[8px]">
            {summary && (
              <>
                {summary.objectCount.toLocaleString()} objects · {formatBytes(summary.totalBytes)}
              </>
            )}
            {settings?.publicRead && <Tag tone="warn">public read</Tag>}
            {settings?.versioning && <Tag tone="accent">versioning on</Tag>}
          </span>
        }
      />

      <Tabs
        tabs={[
          { id: "objects", label: "Objects" },
          { id: "versions", label: "Versions" },
          { id: "settings", label: "Settings" },
        ]}
        active={tab}
        onChange={setTab}
      />

      {tab === "objects" && (
        <ObjectBrowser
          bucket={bucket}
          prefix={prefix}
          versioningOn={settings?.versioning ?? false}
          onMutated={loadContext}
        />
      )}
      {tab === "versions" && (
        <BucketVersionsTab
          bucket={bucket}
          versioningOn={settings?.versioning ?? false}
          onChanged={loadContext}
        />
      )}
      {tab === "settings" && <BucketSettingsTab bucket={bucket} onSaved={loadContext} />}
    </>
  );
}
