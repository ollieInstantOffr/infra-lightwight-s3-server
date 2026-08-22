import { useEffect, useState } from "react";
import type { ObjectVersion, StoredObject } from "../lib/api";
import { api } from "../lib/api";
import { formatBytes, formatDate, formatRelative, previewKind } from "../lib/format";
import {
  Button,
  CopyButton,
  Drawer,
  ErrorNotice,
  InlineSpinner,
  RowAction,
  Spinner,
  Tag,
} from "./ui";

// The preview drawer: what this object is, what it looks like, and what can be
// done with it — without leaving the listing behind.

export function ObjectDrawer({
  bucket,
  object,
  versioningOn,
  onClose,
  onShare,
  onChanged,
}: {
  bucket: string;
  object: StoredObject;
  versioningOn: boolean;
  onClose: () => void;
  onShare: () => void;
  onChanged: () => void;
}) {
  const kind = previewKind(object.contentType);
  const url = objectURL(bucket, object.key, false);

  return (
    <Drawer
      title={object.name || object.key}
      subtitle={
        <>
          {formatBytes(object.size)} · {object.contentType}
        </>
      }
      onClose={onClose}
    >
      <div className="space-y-[18px]">
        <Preview kind={kind} url={url} name={object.name} />

        <dl className="grid grid-cols-[92px_1fr] gap-x-[12px] gap-y-[7px] text-[12.5px]">
          <dt className="text-ink-muted">Key</dt>
          <dd className="m-0 break-all font-mono text-[11.5px]">{object.key}</dd>
          <dt className="text-ink-muted">Size</dt>
          <dd className="m-0 tabular-nums">{formatBytes(object.size)}</dd>
          <dt className="text-ink-muted">Type</dt>
          <dd className="m-0">{object.contentType}</dd>
          <dt className="text-ink-muted">ETag</dt>
          <dd className="m-0 break-all font-mono text-[11.5px]">{object.etag}</dd>
          <dt className="text-ink-muted">Modified</dt>
          <dd className="m-0">{formatDate(object.lastModified)}</dd>
        </dl>

        <div className="flex flex-wrap gap-[8px]">
          <a
            className="inline-flex items-center rounded-[10px] bg-accent px-[14px] py-[9px] text-[13px] font-semibold text-on-accent hover:bg-accent-hover hover:text-white"
            href={objectURL(bucket, object.key, true)}
            download={object.name}
          >
            Download
          </a>
          <Button onClick={onShare}>Share link</Button>
          <CopyButton text={`s3://${bucket}/${object.key}`}>Copy S3 URI</CopyButton>
        </div>

        {versioningOn && (
          <VersionHistory bucket={bucket} objectKey={object.key} onChanged={onChanged} />
        )}
      </div>
    </Drawer>
  );
}

export function objectURL(bucket: string, key: string, download: boolean): string {
  const base = `/api/buckets/${encodeURIComponent(bucket)}/object?key=${encodeURIComponent(key)}`;
  return download ? `${base}&download=1` : base;
}

function Preview({ kind, url, name }: { kind: ReturnType<typeof previewKind>; url: string; name: string }) {
  if (kind === "image") {
    return (
      <img
        src={url}
        alt={name}
        className="max-h-[220px] w-full rounded-[12px] border border-line bg-inset object-contain"
      />
    );
  }
  if (kind === "video") {
    return <video src={url} controls className="max-h-[220px] w-full rounded-[12px] border border-line" />;
  }
  if (kind === "audio") return <audio src={url} controls className="w-full" />;
  if (kind === "pdf") {
    return <iframe title={name} src={url} className="h-[280px] w-full rounded-[12px] border border-line" />;
  }
  if (kind === "text") return <TextPreview url={url} />;

  return (
    <div className="flex h-[110px] items-center justify-center rounded-[12px] border border-dashed border-line bg-inset text-[12.5px] text-ink-faint">
      No preview for this type
    </div>
  );
}

function TextPreview({ url }: { url: string }) {
  const [text, setText] = useState<string | null>(null);
  const [partial, setPartial] = useState(false);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    // Bounded: previewing a multi-gigabyte log would hang the tab.
    fetch(url, { signal: controller.signal, headers: { Range: "bytes=0-65535" } })
      .then(async (response) => {
        setText(await response.text());
        setPartial(response.status === 206);
      })
      .catch(() => setFailed(true));
    return () => controller.abort();
  }, [url]);

  if (failed) return <ErrorNotice message="Could not load a preview." />;
  if (text === null) return <div className="py-[20px]"><InlineSpinner label="Loading preview…" /></div>;

  return (
    <div>
      <pre className="m-0 max-h-[220px] overflow-auto rounded-[12px] border border-line bg-inset p-[13px] font-mono text-[11.5px] leading-[1.7]">
        <code>{text}</code>
      </pre>
      {partial && <p className="mt-[6px] text-[11px] text-ink-faint">Showing the first 64 KB.</p>}
    </div>
  );
}

function VersionHistory({
  bucket,
  objectKey,
  onChanged,
}: {
  bucket: string;
  objectKey: string;
  onChanged: () => void;
}) {
  const [versions, setVersions] = useState<ObjectVersion[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = () => {
    api
      .get<{ versions: ObjectVersion[] }>(
        `/api/buckets/${encodeURIComponent(bucket)}/versions?key=${encodeURIComponent(objectKey)}`,
      )
      .then((result) => setVersions(result.versions))
      .catch((caught: unknown) =>
        setError(caught instanceof Error ? caught.message : "Could not load history."),
      );
  };

  useEffect(load, [bucket, objectKey]);

  async function restore(versionId: string) {
    setBusy(true);
    try {
      await api.post(`/api/buckets/${encodeURIComponent(bucket)}/versions/restore`, {
        key: objectKey,
        versionId,
      });
      load();
      onChanged();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not restore that version.");
    } finally {
      setBusy(false);
    }
  }

  if (error) return <ErrorNotice message={error} />;
  if (versions === null) return <Spinner label="Loading history" />;
  if (versions.length === 0) {
    return (
      <div>
        <h3 className="m-0 mb-[8px] text-[13px] font-semibold">History</h3>
        <p className="m-0 text-[12.5px] text-ink-muted">
          No previous versions yet. One is kept each time this object is replaced or deleted.
        </p>
      </div>
    );
  }

  return (
    <div>
      <h3 className="m-0 mb-[8px] text-[13px] font-semibold">History</h3>
      <div className="rounded-[12px] border border-line">
        {versions.map((version) => (
          <div
            key={version.versionId}
            className="flex items-center gap-[10px] border-b border-line-row px-[12px] py-[10px] last:border-0"
          >
            <div className="min-w-0 flex-1">
              <div className="flex items-center gap-[6px]">
                {version.isDeleteMarker ? (
                  <Tag tone="danger">deleted</Tag>
                ) : version.isCurrent ? (
                  <Tag tone="accent">current</Tag>
                ) : (
                  <Tag>{formatBytes(version.size)}</Tag>
                )}
                <span className="truncate text-[12px] text-ink-muted">
                  {formatRelative(version.createdAt)}
                </span>
              </div>
              <div className="mt-[3px] truncate font-mono text-[10.5px] text-ink-faint">
                {version.createdBy || "unknown"} · {version.versionId.slice(0, 12)}
              </div>
            </div>
            {!version.isDeleteMarker && !version.isCurrent && (
              <RowAction onClick={() => void restore(version.versionId)} title="Make this version current">
                {busy ? "…" : "Restore"}
              </RowAction>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
