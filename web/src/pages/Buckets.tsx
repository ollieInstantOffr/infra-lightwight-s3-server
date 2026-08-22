import { useState } from "react";
import { Link } from "react-router-dom";
import { api, ApiError, type Bucket } from "../lib/api";
import { useApi } from "../lib/useApi";
import { formatBytes, formatDate } from "../lib/format";
import {
  Button,
  Card,
  ConfirmByName,
  EmptyState,
  ErrorNotice,
  Field,
  Modal,
  PageHeader,
  RowAction,
  SkeletonLine,
  TableHead,
  TableRow,
  TextInput,
} from "../components/ui";

// S3's naming rules, checked while typing so the error arrives before the
// request rather than after it. The server validates independently; this is
// for the person, not for safety.
function bucketNameProblem(name: string): string | null {
  if (name === "") return null;
  if (name.length < 3) return "At least 3 characters.";
  if (name.length > 63) return "At most 63 characters.";
  if (name !== name.toLowerCase()) return "Lowercase only.";
  if (!/^[a-z0-9][a-z0-9.-]*[a-z0-9]$/.test(name)) {
    return "Letters, numbers, hyphens and dots. Must start and end with a letter or number.";
  }
  if (name.includes("..")) return "No two dots in a row.";
  if (name.includes(".-") || name.includes("-.")) return "No dot next to a hyphen.";
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(name)) return "Cannot look like an IP address.";
  return null;
}

const columns = "grid-cols-[2.4fr_1fr_.8fr_.8fr_auto]";

export function BucketsPage() {
  const { data, error, loading, reload } = useApi<{ buckets: Bucket[] }>("/api/buckets");
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Bucket | null>(null);

  return (
    <>
      <PageHeader
        title="Buckets"
        subtitle="Each bucket is a separate namespace for objects."
        actions={
          <Button variant="primary" onClick={() => setCreating(true)}>
            New bucket
          </Button>
        }
      />

      {error && <ErrorNotice message={error} onRetry={reload} />}

      <Card className="overflow-hidden">
        <TableHead columns={["Name", "Created", "Objects", "Size", ""]} className={columns} />

        {loading &&
          Array.from({ length: 4 }, (_, index) => (
            <div key={index} className={`grid ${columns} gap-[10px] border-b border-line-row px-[18px] py-[13px]`}>
              <SkeletonLine width="55%" />
              <SkeletonLine width={70} faint />
              <SkeletonLine width={40} faint />
              <SkeletonLine width={52} faint />
              <span />
            </div>
          ))}

        {data?.buckets.map((bucket) => (
          <TableRow key={bucket.name} className={columns}>
            <Link
              to={`/buckets/${encodeURIComponent(bucket.name)}`}
              className="truncate font-mono text-[12.5px] font-medium underline-offset-2 hover:underline"
            >
              {bucket.name}
            </Link>
            <span className="text-[12.5px] text-ink-muted">{formatDate(bucket.createdAt)}</span>
            <span className="tabular-nums text-ink-muted">{bucket.objectCount.toLocaleString()}</span>
            <span className="tabular-nums text-ink-muted">{formatBytes(bucket.totalBytes)}</span>
            <span className="text-right">
              <RowAction danger onClick={() => setDeleting(bucket)}>
                Delete
              </RowAction>
            </span>
          </TableRow>
        ))}

        {data && data.buckets.length === 0 && (
          <EmptyState
            title="No buckets yet"
            hint="A bucket holds objects. Create one to start uploading."
            action={
              <Button variant="primary" onClick={() => setCreating(true)}>
                New bucket
              </Button>
            }
          />
        )}
      </Card>

      {creating && (
        <CreateBucketModal
          onClose={() => setCreating(false)}
          onCreated={() => {
            setCreating(false);
            reload();
          }}
        />
      )}

      {deleting && (
        <DeleteBucketModal
          bucket={deleting}
          onClose={() => setDeleting(null)}
          onDeleted={() => {
            setDeleting(null);
            reload();
          }}
        />
      )}
    </>
  );
}

function CreateBucketModal({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const problem = bucketNameProblem(name);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError(null);
    try {
      await api.post("/api/buckets", { name });
      onCreated();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not create the bucket.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal
      title="Create a bucket"
      subtitle="Names follow S3's rules, so a bucket made here works against real S3 too."
      onClose={onClose}
    >
      <form className="space-y-[16px]" onSubmit={submit}>
        <Field
          label="Name"
          hint="Lowercase letters, numbers, hyphens and dots. This cannot be changed later."
          error={problem}
        >
          <TextInput value={name} onChange={setName} placeholder="my-bucket" autoFocus required mono />
        </Field>
        {error && <ErrorNotice message={error} />}
        <div className="flex justify-end gap-[8px]">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={saving || name === "" || problem !== null}>
            {saving ? "Creating…" : "Create bucket"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

function DeleteBucketModal({
  bucket,
  onClose,
  onDeleted,
}: {
  bucket: Bucket;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  async function remove() {
    setDeleting(true);
    setError(null);
    try {
      await api.delete(`/api/buckets/${encodeURIComponent(bucket.name)}`);
      onDeleted();
    } catch (caught) {
      setError(caught instanceof ApiError ? caught.message : "Could not delete the bucket.");
    } finally {
      setDeleting(false);
    }
  }

  return (
    <Modal title={`Delete ${bucket.name}`} onClose={onClose}>
      <div className="space-y-[16px]">
        <p className="m-0 text-[13px] leading-[1.6]">
          {bucket.objectCount > 0 ? (
            <>
              This bucket holds{" "}
              <span className="font-semibold">{bucket.objectCount.toLocaleString()} objects</span> (
              {formatBytes(bucket.totalBytes)}). Empty it before deleting.
            </>
          ) : (
            <>This bucket is empty. Deleting it cannot be undone.</>
          )}
        </p>
        <ConfirmByName name={bucket.name} typed={typed} onTyped={setTyped} />
        {error && <ErrorNotice message={error} />}
        <div className="flex justify-end gap-[8px]">
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" onClick={remove} disabled={deleting || typed !== bucket.name}>
            {deleting ? "Deleting…" : "Delete bucket"}
          </Button>
        </div>
      </div>
    </Modal>
  );
}
