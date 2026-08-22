import { useCallback, useEffect, useState } from "react";
import { api, type ObjectVersion } from "../../lib/api";
import { formatBytes, formatDate } from "../../lib/format";
import {
  Button,
  Card,
  EmptyState,
  ErrorNotice,
  InfoNotice,
  Modal,
  RowAction,
  Spinner,
  Tag,
  TableHead,
  TableRow,
} from "../../components/ui";

// Version history across a bucket: what changed, who changed it, and the two
// destructive operations — restore and purge — that only make sense here.

const columns = "grid-cols-[2.4fr_.8fr_.8fr_1fr_1fr_auto]";

export function BucketVersionsTab({
  bucket,
  versioningOn,
  onChanged,
}: {
  bucket: string;
  versioningOn: boolean;
  onChanged: () => void;
}) {
  const [versions, setVersions] = useState<ObjectVersion[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [purging, setPurging] = useState<ObjectVersion | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api
      .get<{ versions: ObjectVersion[] }>(`/api/buckets/${encodeURIComponent(bucket)}/versions`)
      .then((result) => setVersions(result.versions))
      .catch((caught: unknown) =>
        setError(caught instanceof Error ? caught.message : "Could not load version history."),
      );
  }, [bucket]);

  useEffect(load, [load]);

  async function restore(version: ObjectVersion) {
    setBusy(true);
    try {
      await api.post(`/api/buckets/${encodeURIComponent(bucket)}/versions/restore`, {
        key: version.key,
        versionId: version.versionId,
      });
      load();
      onChanged();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not restore that version.");
    } finally {
      setBusy(false);
    }
  }

  async function purge(version: ObjectVersion, wholeKey: boolean) {
    setBusy(true);
    try {
      await api.post(`/api/buckets/${encodeURIComponent(bucket)}/versions/purge`, {
        key: version.key,
        versionId: wholeKey ? "" : version.versionId,
      });
      setPurging(null);
      load();
      onChanged();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not purge.");
    } finally {
      setBusy(false);
    }
  }

  if (!versioningOn) {
    return (
      <Card>
        <EmptyState
          title="Versioning is off for this bucket"
          hint="With versioning on, overwriting or deleting an object keeps the previous state so it can be restored. Turn it on in Settings."
        />
      </Card>
    );
  }

  if (error && !versions) return <ErrorNotice message={error} />;
  if (!versions) return <Spinner label="Loading version history" />;

  return (
    <div className="space-y-[14px]">
      <InfoNotice>
        Versions hold their own copy of the bytes, so space is not reclaimed until they are purged.
        Purging is permanent — it is the one action here that cannot be undone by another restore.
      </InfoNotice>

      {error && <ErrorNotice message={error} />}

      <Card className="overflow-hidden">
        <TableHead
          columns={["Key", "State", "Size", "When", "By", ""]}
          className={columns}
        />

        {versions.map((version) => (
          <TableRow key={version.versionId} className={columns}>
            <span className="truncate font-mono text-[12.5px]">{version.key}</span>
            <span>
              {version.isDeleteMarker ? (
                <Tag tone="danger">deleted</Tag>
              ) : version.isCurrent ? (
                <Tag tone="accent">current</Tag>
              ) : (
                <Tag>superseded</Tag>
              )}
            </span>
            <span className="tabular-nums text-[12.5px] text-ink-muted">
              {version.isDeleteMarker ? "—" : formatBytes(version.size)}
            </span>
            <span className="text-[12.5px] text-ink-muted">{formatDate(version.createdAt)}</span>
            <span className="truncate font-mono text-[11.5px] text-ink-faint">
              {version.createdBy || "unknown"}
            </span>
            <span className="flex justify-end gap-[2px]">
              {!version.isDeleteMarker && !version.isCurrent && (
                <RowAction onClick={() => void restore(version)} title="Make this version current">
                  {busy ? "…" : "Restore"}
                </RowAction>
              )}
              <RowAction danger onClick={() => setPurging(version)}>
                Purge
              </RowAction>
            </span>
          </TableRow>
        ))}

        {versions.length === 0 && (
          <EmptyState
            title="No history yet"
            hint="A version is kept each time an object here is replaced or deleted."
          />
        )}
      </Card>

      {purging && (
        <Modal title="Purge versions" subtitle={purging.key} onClose={() => setPurging(null)}>
          <div className="space-y-[16px]">
            <p className="m-0 text-[13px] leading-[1.6]">
              Purging permanently removes history and reclaims the space. Unlike a delete, this
              cannot be undone by restoring.
            </p>
            <div className="flex flex-wrap justify-end gap-[8px]">
              <Button onClick={() => setPurging(null)}>Cancel</Button>
              <Button variant="danger" onClick={() => void purge(purging, false)} disabled={busy}>
                Purge this version
              </Button>
              <Button variant="danger" onClick={() => void purge(purging, true)} disabled={busy}>
                Purge all history for this key
              </Button>
            </div>
          </div>
        </Modal>
      )}
    </div>
  );
}
