import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, api, uploadObject, type ObjectListing, type StoredObject } from "../../lib/api";
import { formatBytes, formatRelative } from "../../lib/format";
import {
  Button,
  Card,
  EmptyState,
  ErrorNotice,
  Field,
  InlineSpinner,
  Modal,
  ProgressBar,
  RowAction,
  SkeletonLine,
  Tag,
  TableHead,
  TableRow,
  TextInput,
} from "../../components/ui";
import { ObjectDrawer, objectURL } from "../../components/ObjectDrawer";
import { ShareModal } from "../../components/ShareModal";

// The object browser. An object store has no folders — a folder is a common
// prefix that exists because objects sit under it — so navigating is really
// changing which prefix is listed. The prefix lives in the URL, so the back
// button and bookmarks behave the way people expect them to.

type Upload = { name: string; progress: number; error?: string };

const columns = "grid-cols-[24px_2.6fr_.7fr_.8fr_1fr_auto]";

export function ObjectBrowser({
  bucket,
  prefix,
  versioningOn,
  onMutated,
}: {
  bucket: string;
  prefix: string;
  versioningOn: boolean;
  // Anything that adds or removes objects changes the bucket's own totals,
  // which live in the header above this component. Without telling the parent,
  // the header keeps claiming "4 objects" after one has been deleted.
  onMutated: () => void;
}) {
  const [listing, setListing] = useState<ObjectListing | null>(null);
  const [loading, setLoading] = useState(true);
  const [slow, setSlow] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [uploads, setUploads] = useState<Upload[]>([]);
  const [inspecting, setInspecting] = useState<StoredObject | null>(null);
  const [sharing, setSharing] = useState<string | null>(null);
  const [confirming, setConfirming] = useState<null | { keys: string[]; prefix?: string }>(null);
  const [creatingFolder, setCreatingFolder] = useState(false);
  const [dragging, setDragging] = useState(false);
  const [filter, setFilter] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    // A listing that takes more than a moment says so, rather than leaving a
    // skeleton that could equally mean "broken".
    const slowTimer = window.setTimeout(() => setSlow(true), 1200);
    try {
      const query = new URLSearchParams();
      if (prefix) query.set("prefix", prefix);
      setListing(
        await api.get<ObjectListing>(
          `/api/buckets/${encodeURIComponent(bucket)}/objects?${query.toString()}`,
        ),
      );
      setSelected(new Set());
    } catch (caught) {
      setError(
        caught instanceof ApiError && caught.status === 404
          ? "That bucket no longer exists."
          : caught instanceof Error
            ? caught.message
            : "Could not list objects.",
      );
    } finally {
      window.clearTimeout(slowTimer);
      setSlow(false);
      setLoading(false);
    }
  }, [bucket, prefix]);

  useEffect(() => {
    void load();
  }, [load]);

  const startUploads = useCallback(
    async (files: File[]) => {
      // Sequential on purpose. The console path is for convenience; saturating
      // a small node with parallel browser uploads is not what it is for, and
      // anything large should go through a presigned URL where multipart
      // handles it.
      for (const file of files) {
        setUploads((current) => [...current, { name: file.name, progress: 0 }]);
        try {
          await uploadObject(bucket, prefix + file.name, file, (fraction) => {
            setUploads((current) =>
              current.map((upload) =>
                upload.name === file.name ? { ...upload, progress: fraction } : upload,
              ),
            );
          });
        } catch (caught) {
          setUploads((current) =>
            current.map((upload) =>
              upload.name === file.name
                ? { ...upload, error: caught instanceof Error ? caught.message : "Upload failed." }
                : upload,
            ),
          );
        }
      }
      // Failures stay on screen so the message can be read; successes clear.
      setUploads((current) => current.filter((upload) => upload.error));
      await load();
      onMutated();
    },
    [bucket, prefix, load, onMutated],
  );

  async function remove(keys: string[], folderPrefix?: string) {
    try {
      await api.post(`/api/buckets/${encodeURIComponent(bucket)}/objects/delete`,
        folderPrefix ? { prefix: folderPrefix } : { keys });
      await load();
      onMutated();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not delete.");
    }
  }

  const visibleObjects =
    listing?.objects.filter((object) => object.name.toLowerCase().includes(filter.toLowerCase())) ?? [];
  const visibleFolders =
    listing?.folders.filter((folder) => folder.name.toLowerCase().includes(filter.toLowerCase())) ?? [];

  return (
    <div
      onDragOver={(event) => {
        event.preventDefault();
        setDragging(true);
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(event) => {
        event.preventDefault();
        setDragging(false);
        const files = Array.from(event.dataTransfer.files);
        if (files.length > 0) void startUploads(files);
      }}
    >
      <div className="mb-[14px] flex flex-wrap items-center justify-between gap-[10px]">
        <div className="flex items-center gap-[8px] text-[12.5px]">
          <span className="text-ink-faint">Prefix</span>
          <span className="rounded-[8px] border border-line bg-inset px-[9px] py-[5px] font-mono text-[12px]">
            /{prefix}
          </span>
        </div>
        <div className="flex flex-wrap items-center gap-[8px]">
          <div className="w-[220px]">
            <TextInput value={filter} onChange={setFilter} placeholder="Filter this folder" ariaLabel="Filter by name" />
          </div>
          <Button onClick={() => setCreatingFolder(true)}>New folder</Button>
          <UploadButton onFiles={startUploads} />
        </div>
      </div>

      {dragging && (
        <div className="mb-[14px] rounded-[16px] border-2 border-dashed border-accent bg-accent-soft px-[18px] py-[26px] text-center text-[13px] font-medium text-accent-deep">
          Drop files to upload into /{prefix}
        </div>
      )}

      {uploads.length > 0 && (
        <Card className="mb-[14px] overflow-hidden">
          {uploads.map((upload) => (
            <div key={upload.name} className="border-b border-line-row px-[16px] py-[12px] last:border-0">
              <div className="mb-[7px] flex items-baseline justify-between gap-[12px] text-[12.5px]">
                <span className="truncate font-mono">{upload.name}</span>
                <span className="flex-none text-ink-faint">
                  {upload.error ? "failed" : `${Math.round(upload.progress * 100)}%`}
                </span>
              </div>
              {upload.error ? (
                <p className="m-0 text-[12px] text-danger">{upload.error}</p>
              ) : (
                <ProgressBar fraction={upload.progress} label={`Uploading ${upload.name}`} />
              )}
            </div>
          ))}
        </Card>
      )}

      {error && <ErrorNotice message={error} onRetry={load} />}

      {selected.size > 0 && (
        <div className="mb-[12px] flex items-center gap-[10px] rounded-[12px] border border-line bg-inset px-[14px] py-[10px]">
          <span className="text-[12.5px] font-medium">
            {selected.size} selected
          </span>
          <Button variant="danger" onClick={() => setConfirming({ keys: Array.from(selected) })}>
            Delete
          </Button>
          <Button onClick={() => setSelected(new Set())}>Clear</Button>
        </div>
      )}

      <Card className="overflow-hidden">
        <TableHead columns={["", "Key", "Type", "Size", "Modified", ""]} className={columns} />

        {loading &&
          Array.from({ length: 5 }, (_, index) => (
            <div key={index} className={`grid ${columns} items-center gap-[10px] border-b border-line-row px-[18px] py-[13px]`}>
              <SkeletonLine width={13} height={13} />
              <SkeletonLine width={`${60 - index * 6}%`} />
              <SkeletonLine width={30} faint />
              <SkeletonLine width={42} faint />
              <SkeletonLine width={54} faint />
              <span />
            </div>
          ))}

        {/* Going up is a row rather than a button, so it sits where the eye
            already is when navigating a hierarchy. */}
        {!loading && prefix !== "" && (
          <TableRow className={columns}>
            <span />
            <Link
              to={`/buckets/${encodeURIComponent(bucket)}/${parentPrefix(prefix)}`}
              className="font-mono text-[12.5px] text-ink-muted underline-offset-2 hover:underline"
            >
              ../
            </Link>
            <span />
            <span />
            <span />
            <span />
          </TableRow>
        )}

        {visibleFolders.map((folder) => (
          <TableRow key={folder.prefix} className={columns}>
            <span />
            <Link
              to={`/buckets/${encodeURIComponent(bucket)}/${folder.prefix}`}
              className="truncate font-mono text-[12.5px] font-medium underline-offset-2 hover:underline"
            >
              {folder.name}/
            </Link>
            <Tag>folder</Tag>
            <span />
            <span />
            <span className="text-right">
              <RowAction danger onClick={() => setConfirming({ keys: [], prefix: folder.prefix })}>
                Delete
              </RowAction>
            </span>
          </TableRow>
        ))}

        {visibleObjects.map((object) => (
          <TableRow key={object.key} className={columns}>
            <input
              type="checkbox"
              aria-label={`Select ${object.name}`}
              checked={selected.has(object.key)}
              onChange={(event) => {
                const next = new Set(selected);
                if (event.target.checked) next.add(object.key);
                else next.delete(object.key);
                setSelected(next);
              }}
            />
            <button
              className="truncate text-left font-mono text-[12.5px] underline-offset-2 hover:underline"
              onClick={() => setInspecting(object)}
            >
              {object.name}
            </button>
            <span>
              <Tag tone="mono">{extensionOf(object.name, object.contentType)}</Tag>
            </span>
            <span className="tabular-nums text-[12.5px] text-ink-muted">{formatBytes(object.size)}</span>
            <span className="text-[12.5px] text-ink-muted">{formatRelative(object.lastModified)}</span>
            <span className="flex justify-end gap-[2px]">
              <a
                className="rounded-md px-2 py-1 text-[12.5px] font-medium text-ink-muted hover:bg-inset hover:text-ink"
                href={objectURL(bucket, object.key, true)}
                download={object.name}
              >
                Download
              </a>
              <RowAction onClick={() => setSharing(object.key)}>Share</RowAction>
              {/* On the row, not only behind a checkbox. Folder rows have had a
                  Delete since the start, and having objects behave differently
                  made the capability look absent rather than hidden. */}
              <RowAction danger onClick={() => setConfirming({ keys: [object.key] })}>
                Delete
              </RowAction>
            </span>
          </TableRow>
        ))}

        {!loading && visibleFolders.length === 0 && visibleObjects.length === 0 && (
          <EmptyState
            title={filter ? "Nothing matches that filter" : prefix ? "This folder is empty" : "This bucket is empty"}
            hint={filter ? undefined : "Drag files onto the page, or use the upload button."}
          />
        )}

        {slow && (
          <div className="border-t border-line-row bg-inset px-[16px] py-[11px]">
            <InlineSpinner label="Still listing — this bucket has a lot of keys" />
          </div>
        )}

        {listing?.isTruncated && (
          <div className="border-t border-line-row px-[16px] py-[11px] text-[11.5px] text-ink-faint">
            Showing the first {listing.objects.length + listing.folders.length} entries in this folder.
            Narrow the prefix, or use search, to reach the rest.
          </div>
        )}
      </Card>

      {inspecting && (
        <ObjectDrawer
          bucket={bucket}
          object={inspecting}
          versioningOn={versioningOn}
          onClose={() => setInspecting(null)}
          onShare={() => {
            setSharing(inspecting.key);
            setInspecting(null);
          }}
          onDelete={() => {
            // The drawer describes an object that is about to stop existing,
            // so it closes before the confirmation opens.
            setConfirming({ keys: [inspecting.key] });
            setInspecting(null);
          }}
          onChanged={load}
        />
      )}

      {sharing && <ShareModal bucket={bucket} objectKey={sharing} onClose={() => setSharing(null)} />}

      {creatingFolder && (
        <NewFolderModal
          bucket={bucket}
          prefix={prefix}
          onClose={() => setCreatingFolder(false)}
          onCreated={() => {
            setCreatingFolder(false);
            void load();
            onMutated();
          }}
        />
      )}

      {confirming && (
        <Modal
          title={
            confirming.prefix
              ? "Delete folder"
              : confirming.keys.length === 1
                ? "Delete object"
                : `Delete ${confirming.keys.length} objects`
          }
          subtitle={
            !confirming.prefix && confirming.keys.length === 1 ? confirming.keys[0] : undefined
          }
          onClose={() => setConfirming(null)}
        >
          <div className="space-y-[16px]">
            <p className="m-0 text-[13px] leading-[1.6]">
              {confirming.prefix ? (
                <>
                  Everything under <span className="font-mono">{confirming.prefix}</span> will be deleted.
                </>
              ) : confirming.keys.length === 1 ? (
                <>This removes the object.</>
              ) : (
                <>This removes the selected objects.</>
              )}{" "}
              {versioningOn
                ? "Versioning is on, so the previous state is kept as history and the space is not reclaimed until it is purged."
                : "There is only one copy of each object, so this cannot be undone."}
            </p>
            <div className="flex justify-end gap-[8px]">
              <Button onClick={() => setConfirming(null)}>Cancel</Button>
              <Button
                variant="danger"
                onClick={() => {
                  const target = confirming;
                  setConfirming(null);
                  void remove(target.keys, target.prefix);
                }}
              >
                Delete
              </Button>
            </div>
          </div>
        </Modal>
      )}
    </div>
  );
}

function UploadButton({ onFiles }: { onFiles: (files: File[]) => void }) {
  const input = useRef<HTMLInputElement>(null);
  return (
    <>
      <input
        ref={input}
        type="file"
        multiple
        className="hidden"
        onChange={(event) => {
          const files = Array.from(event.target.files ?? []);
          if (files.length > 0) onFiles(files);
          // Cleared so choosing the same file twice in a row still fires.
          event.target.value = "";
        }}
      />
      <Button variant="primary" onClick={() => input.current?.click()}>
        Upload
      </Button>
    </>
  );
}

function NewFolderModal({
  bucket,
  prefix,
  onClose,
  onCreated,
}: {
  bucket: string;
  prefix: string;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError(null);
    try {
      await api.post(`/api/buckets/${encodeURIComponent(bucket)}/folders`, { prefix: prefix + name });
      onCreated();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not create the folder.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal
      title="New folder"
      subtitle="Object stores have no folders. This writes a zero-byte marker so the folder appears while it is empty."
      onClose={onClose}
    >
      <form className="space-y-[16px]" onSubmit={submit}>
        <Field label="Name" hint={`Created under /${prefix}`}>
          <TextInput value={name} onChange={setName} placeholder="reports" autoFocus required mono />
        </Field>
        {error && <ErrorNotice message={error} />}
        <div className="flex justify-end gap-[8px]">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={saving || name.trim() === ""}>
            {saving ? "Creating…" : "Create folder"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

/** The prefix one level up, for the "../" row. */
function parentPrefix(prefix: string): string {
  const trimmed = prefix.endsWith("/") ? prefix.slice(0, -1) : prefix;
  const index = trimmed.lastIndexOf("/");
  return index === -1 ? "" : trimmed.slice(0, index + 1);
}

/** A short type label: the extension where there is one, else the subtype. */
function extensionOf(name: string, contentType: string): string {
  const dot = name.lastIndexOf(".");
  if (dot > 0 && dot < name.length - 1) return name.slice(dot + 1).toLowerCase().slice(0, 8);
  const slash = contentType.indexOf("/");
  return slash === -1 ? contentType : contentType.slice(slash + 1).slice(0, 8);
}
