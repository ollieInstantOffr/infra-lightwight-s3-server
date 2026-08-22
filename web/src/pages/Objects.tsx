import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api, ApiError, uploadObject, type ObjectListing, type StoredObject } from "../lib/api";
import { breadcrumbs, formatBytes, formatDate, previewKind } from "../lib/format";
import {
  Button,
  Card,
  CopyButton,
  EmptyState,
  ErrorNotice,
  Field,
  Modal,
  Spinner,
  TextInput,
} from "../components/ui";

// The object browser. An object store has no folders — a "folder" is a common
// prefix produced by a delimiter — so navigation is really just changing which
// prefix is being listed. The URL carries that prefix so the browser's back
// button and bookmarks work as people expect.

type UploadState = {
  name: string;
  progress: number;
  error?: string;
};

export function ObjectsPage() {
  const { bucket = "", "*": rawPrefix = "" } = useParams();
  const navigate = useNavigate();
  const prefix = rawPrefix;

  const [listing, setListing] = useState<ObjectListing | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [uploads, setUploads] = useState<UploadState[]>([]);
  const [inspecting, setInspecting] = useState<StoredObject | null>(null);
  const [sharing, setSharing] = useState<StoredObject | null>(null);
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const [dragging, setDragging] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
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
      if (caught instanceof ApiError && caught.status === 404) {
        setError("That bucket no longer exists.");
      } else {
        setError(caught instanceof Error ? caught.message : "Could not list objects.");
      }
    } finally {
      setLoading(false);
    }
  }, [bucket, prefix]);

  useEffect(() => {
    void load();
  }, [load]);

  const startUploads = useCallback(
    async (files: File[]) => {
      // Uploaded one at a time on purpose. The console path is for
      // convenience; saturating a small server with parallel large uploads
      // from a browser is not what it is for, and large files should go
      // through a presigned URL anyway.
      for (const file of files) {
        const key = prefix + file.name;
        setUploads((current) => [...current, { name: file.name, progress: 0 }]);
        try {
          await uploadObject(bucket, key, file, (fraction) => {
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
      // Failed uploads stay on screen so the message can be read; successful
      // ones clear themselves.
      setUploads((current) => current.filter((upload) => upload.error));
      await load();
    },
    [bucket, prefix, load],
  );

  const crumbs = breadcrumbs(prefix);

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
      className={dragging ? "rounded-lg outline-2 outline-dashed outline-accent" : ""}
    >
      <header className="mb-6 flex flex-wrap items-start justify-between gap-4">
        <div>
          <nav className="flex flex-wrap items-center gap-1 text-sm" aria-label="Breadcrumb">
            <Link to="/buckets" className="text-ink-muted underline-offset-2 hover:underline">
              Buckets
            </Link>
            <span className="text-ink-muted">/</span>
            <Link
              to={`/buckets/${encodeURIComponent(bucket)}`}
              className="font-medium underline-offset-2 hover:underline"
            >
              {bucket}
            </Link>
            {crumbs.map((crumb) => (
              <span key={crumb.prefix} className="flex items-center gap-1">
                <span className="text-ink-muted">/</span>
                <Link
                  to={`/buckets/${encodeURIComponent(bucket)}/${crumb.prefix}`}
                  className="underline-offset-2 hover:underline"
                >
                  {crumb.name}
                </Link>
              </span>
            ))}
          </nav>
          <p className="mt-1 text-sm text-ink-muted">Drag files anywhere on this page to upload.</p>
        </div>

        <div className="flex items-center gap-2">
          {selected.size > 0 && (
            <Button variant="danger" onClick={() => setConfirmingDelete(true)}>
              Delete {selected.size}
            </Button>
          )}
          <UploadButton onFiles={startUploads} />
        </div>
      </header>

      {uploads.length > 0 && (
        <Card className="mb-4 divide-y divide-border">
          {uploads.map((upload) => (
            <div key={upload.name} className="px-4 py-3">
              <div className="flex items-baseline justify-between gap-4 text-sm">
                <span className="truncate">{upload.name}</span>
                <span className="text-ink-muted">
                  {upload.error ? "failed" : `${Math.round(upload.progress * 100)}%`}
                </span>
              </div>
              {upload.error ? (
                <p className="mt-1 text-sm text-danger">{upload.error}</p>
              ) : (
                <div className="mt-2 h-1.5 overflow-hidden rounded-full bg-border">
                  <div
                    className="h-full rounded-full bg-accent transition-[width]"
                    style={{ width: `${upload.progress * 100}%` }}
                  />
                </div>
              )}
            </div>
          ))}
        </Card>
      )}

      {loading && <Spinner label="Loading objects" />}
      {error && <ErrorNotice message={error} onRetry={load} />}

      {listing && !loading && listing.folders.length === 0 && listing.objects.length === 0 && (
        <Card>
          <EmptyState
            title={prefix ? "This folder is empty" : "This bucket is empty"}
            hint="Drag files onto the page, or use the upload button."
          />
        </Card>
      )}

      {listing && (listing.folders.length > 0 || listing.objects.length > 0) && (
        <Card className="overflow-hidden">
          <table className="w-full text-sm">
            <thead className="border-b border-border text-left text-ink-muted">
              <tr>
                <th className="w-10 px-4 py-2">
                  <input
                    type="checkbox"
                    aria-label="Select all objects"
                    checked={selected.size > 0 && selected.size === listing.objects.length}
                    onChange={(event) =>
                      setSelected(
                        event.target.checked ? new Set(listing.objects.map((o) => o.key)) : new Set(),
                      )
                    }
                  />
                </th>
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 text-right font-medium">Size</th>
                <th className="px-4 py-2 font-medium">Modified</th>
                <th className="px-4 py-2" />
              </tr>
            </thead>
            <tbody>
              {listing.folders.map((folder) => (
                <tr key={folder.prefix} className="border-b border-border last:border-0">
                  <td className="px-4 py-2" />
                  <td className="px-4 py-2" colSpan={3}>
                    <Link
                      to={`/buckets/${encodeURIComponent(bucket)}/${folder.prefix}`}
                      className="font-medium underline-offset-2 hover:underline"
                    >
                      {folder.name}/
                    </Link>
                  </td>
                  <td className="px-4 py-2 text-right">
                    <Button
                      variant="ghost"
                      onClick={() => {
                        setSelected(new Set());
                        void deletePrefix(bucket, folder.prefix, load, setError);
                      }}
                    >
                      Delete
                    </Button>
                  </td>
                </tr>
              ))}

              {listing.objects.map((object) => (
                <tr key={object.key} className="border-b border-border last:border-0">
                  <td className="px-4 py-2">
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
                  </td>
                  <td className="px-4 py-2">
                    <button
                      className="text-left underline-offset-2 hover:underline"
                      onClick={() => setInspecting(object)}
                    >
                      {object.name}
                    </button>
                  </td>
                  <td className="px-4 py-2 text-right tabular-nums">{formatBytes(object.size)}</td>
                  <td className="px-4 py-2 text-ink-muted">{formatDate(object.lastModified)}</td>
                  <td className="px-4 py-2">
                    <div className="flex items-center justify-end gap-1">
                      <a
                        className="rounded-md px-3 py-1.5 text-sm text-ink-muted hover:text-ink"
                        href={downloadURL(bucket, object.key)}
                        download={object.name}
                      >
                        Download
                      </a>
                      <Button variant="ghost" onClick={() => setSharing(object)}>
                        Share
                      </Button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          {listing.isTruncated && (
            <div className="border-t border-border px-4 py-3 text-sm text-ink-muted">
              Showing the first {listing.objects.length + listing.folders.length} entries. Narrow the
              prefix to see more.
            </div>
          )}
        </Card>
      )}

      {inspecting && (
        <ObjectDetail
          bucket={bucket}
          object={inspecting}
          onClose={() => setInspecting(null)}
          onShare={() => {
            setSharing(inspecting);
            setInspecting(null);
          }}
        />
      )}

      {sharing && <ShareModal bucket={bucket} object={sharing} onClose={() => setSharing(null)} />}

      {confirmingDelete && (
        <Modal title={`Delete ${selected.size} object${selected.size === 1 ? "" : "s"}`} onClose={() => setConfirmingDelete(false)}>
          <div className="space-y-4">
            <p className="text-sm">This cannot be undone. There is only one copy of each object.</p>
            <div className="flex justify-end gap-2">
              <Button variant="secondary" onClick={() => setConfirmingDelete(false)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                onClick={() => {
                  setConfirmingDelete(false);
                  void deleteKeys(bucket, Array.from(selected), load, setError);
                }}
              >
                Delete
              </Button>
            </div>
          </div>
        </Modal>
      )}

      <p className="mt-4 text-sm text-ink-muted">
        <button className="underline underline-offset-2" onClick={() => navigate(-1)}>
          Back
        </button>
      </p>
    </div>
  );
}

function downloadURL(bucket: string, key: string): string {
  return `/api/buckets/${encodeURIComponent(bucket)}/object?key=${encodeURIComponent(key)}&download=1`;
}

function previewURL(bucket: string, key: string): string {
  return `/api/buckets/${encodeURIComponent(bucket)}/object?key=${encodeURIComponent(key)}`;
}

async function deleteKeys(
  bucket: string,
  keys: string[],
  reload: () => Promise<void>,
  onError: (message: string) => void,
) {
  try {
    await api.post(`/api/buckets/${encodeURIComponent(bucket)}/objects/delete`, { keys });
    await reload();
  } catch (caught) {
    onError(caught instanceof Error ? caught.message : "Could not delete those objects.");
  }
}

async function deletePrefix(
  bucket: string,
  prefix: string,
  reload: () => Promise<void>,
  onError: (message: string) => void,
) {
  if (!window.confirm(`Delete everything under ${prefix}? This cannot be undone.`)) return;
  try {
    await api.post(`/api/buckets/${encodeURIComponent(bucket)}/objects/delete`, { prefix });
    await reload();
  } catch (caught) {
    onError(caught instanceof Error ? caught.message : "Could not delete that folder.");
  }
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
          // Cleared so selecting the same file twice in a row still fires.
          event.target.value = "";
        }}
      />
      <Button onClick={() => input.current?.click()}>Upload</Button>
    </>
  );
}

function ObjectDetail({
  bucket,
  object,
  onClose,
  onShare,
}: {
  bucket: string;
  object: StoredObject;
  onClose: () => void;
  onShare: () => void;
}) {
  const kind = previewKind(object.contentType);
  const url = previewURL(bucket, object.key);

  return (
    <Modal title={object.name} onClose={onClose}>
      <div className="space-y-4">
        {kind === "image" && (
          <img src={url} alt={object.name} className="max-h-64 w-full rounded-md object-contain" />
        )}
        {kind === "video" && <video src={url} controls className="max-h-64 w-full rounded-md" />}
        {kind === "audio" && <audio src={url} controls className="w-full" />}
        {kind === "text" && <TextPreview url={url} />}
        {kind === "pdf" && (
          <iframe title={object.name} src={url} className="h-64 w-full rounded-md border border-border" />
        )}

        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
          <dt className="text-ink-muted">Key</dt>
          <dd className="break-all font-mono text-xs">{object.key}</dd>
          <dt className="text-ink-muted">Size</dt>
          <dd>{formatBytes(object.size)}</dd>
          <dt className="text-ink-muted">Type</dt>
          <dd>{object.contentType}</dd>
          <dt className="text-ink-muted">ETag</dt>
          <dd className="break-all font-mono text-xs">{object.etag}</dd>
          <dt className="text-ink-muted">Modified</dt>
          <dd>{formatDate(object.lastModified)}</dd>
        </dl>

        <div className="flex flex-wrap justify-end gap-2">
          <CopyButton text={`s3://${bucket}/${object.key}`}>Copy S3 URI</CopyButton>
          <Button variant="secondary" onClick={onShare}>
            Share link
          </Button>
          <a
            className="inline-flex items-center rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-accent-ink"
            href={downloadURL(bucket, object.key)}
            download={object.name}
          >
            Download
          </a>
        </div>
      </div>
    </Modal>
  );
}

function TextPreview({ url }: { url: string }) {
  const [text, setText] = useState<string | null>(null);
  const [tooLarge, setTooLarge] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    // Bounded: previewing a multi-gigabyte log file would hang the browser.
    fetch(url, { signal: controller.signal, headers: { Range: "bytes=0-65535" } })
      .then(async (response) => {
        const body = await response.text();
        setText(body);
        setTooLarge(response.status === 206);
      })
      .catch(() => setText(null));
    return () => controller.abort();
  }, [url]);

  if (text === null) return <Spinner label="Loading preview" />;
  return (
    <div>
      <pre className="max-h-64 overflow-auto rounded-md border border-border bg-surface p-3 text-xs leading-relaxed">
        <code>{text}</code>
      </pre>
      {tooLarge && <p className="mt-1 text-xs text-ink-muted">Showing the first 64 KB.</p>}
    </div>
  );
}

function ShareModal({
  bucket,
  object,
  onClose,
}: {
  bucket: string;
  object: StoredObject;
  onClose: () => void;
}) {
  const [hours, setHours] = useState("24");
  const [link, setLink] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  async function create() {
    setCreating(true);
    setError(null);
    try {
      const result = await api.post<{ url: string }>(
        `/api/buckets/${encodeURIComponent(bucket)}/share`,
        { key: object.key, expiresSeconds: Number(hours) * 3600 },
      );
      setLink(result.url);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not create a share link.");
    } finally {
      setCreating(false);
    }
  }

  return (
    <Modal title={`Share ${object.name}`} onClose={onClose}>
      <div className="space-y-4">
        <p className="text-sm text-ink-muted">
          Anyone with the link can download this object until it expires. No sign-in is required.
        </p>
        <Field label="Expires after (hours)" hint="At most 168 hours, which is seven days.">
          <TextInput type="number" value={hours} onChange={setHours} />
        </Field>
        {error && <ErrorNotice message={error} />}
        {link && (
          <div>
            <p className="mb-1 text-xs font-medium text-ink-muted">Share link</p>
            <textarea
              readOnly
              value={link}
              rows={4}
              className="w-full rounded-md border border-border bg-surface p-2 font-mono text-xs"
            />
            <div className="mt-1">
              <CopyButton text={link} />
            </div>
          </div>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
          <Button onClick={create} disabled={creating}>
            {creating ? "Creating…" : link ? "Create another" : "Create link"}
          </Button>
        </div>
      </div>
    </Modal>
  );
}
