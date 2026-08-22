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
  Spinner,
  TextInput,
} from "../components/ui";

// S3's naming rules, checked as the user types so the error arrives before the
// request rather than after it. The server validates independently — this is
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

export function BucketsPage() {
  const { data, error, loading, reload } = useApi<{ buckets: Bucket[] }>("/api/buckets");
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Bucket | null>(null);

  return (
    <>
      <PageHeader
        title="Buckets"
        description="Each bucket is a separate namespace for objects."
        actions={<Button onClick={() => setCreating(true)}>New bucket</Button>}
      />

      {loading && <Spinner label="Loading buckets" />}
      {error && <ErrorNotice message={error} onRetry={reload} />}

      {data && data.buckets.length === 0 && (
        <Card>
          <EmptyState
            title="No buckets yet"
            hint="A bucket holds objects. Create one to start uploading."
            action={<Button onClick={() => setCreating(true)}>New bucket</Button>}
          />
        </Card>
      )}

      {data && data.buckets.length > 0 && (
        <Card className="overflow-hidden">
          <table className="w-full text-sm">
            <thead className="border-b border-border text-left text-ink-muted">
              <tr>
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 text-right font-medium">Objects</th>
                <th className="px-4 py-2 text-right font-medium">Size</th>
                <th className="px-4 py-2 font-medium">Created</th>
                <th className="px-4 py-2" />
              </tr>
            </thead>
            <tbody>
              {data.buckets.map((bucket) => (
                <tr key={bucket.name} className="border-b border-border last:border-0">
                  <td className="px-4 py-2">
                    <Link
                      to={`/buckets/${encodeURIComponent(bucket.name)}`}
                      className="font-medium underline-offset-2 hover:underline"
                    >
                      {bucket.name}
                    </Link>
                  </td>
                  <td className="px-4 py-2 text-right tabular-nums">{bucket.objectCount.toLocaleString()}</td>
                  <td className="px-4 py-2 text-right tabular-nums">{formatBytes(bucket.totalBytes)}</td>
                  <td className="px-4 py-2 text-ink-muted">{formatDate(bucket.createdAt)}</td>
                  <td className="px-4 py-2 text-right">
                    <Button variant="ghost" onClick={() => setDeleting(bucket)}>
                      Delete
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}

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
    <Modal title="New bucket" onClose={onClose}>
      <form className="space-y-4" onSubmit={submit}>
        <Field
          label="Name"
          hint="Lowercase letters, numbers, hyphens and dots. This cannot be changed later."
        >
          <TextInput value={name} onChange={setName} placeholder="my-bucket" autoFocus required />
        </Field>
        {problem && <p className="text-sm text-danger">{problem}</p>}
        {error && <ErrorNotice message={error} />}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={saving || name === "" || problem !== null}>
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
      // A non-empty bucket is the common case here, and the server's message
      // already says so — passing it through is better than inventing one.
      setError(
        caught instanceof ApiError ? caught.message : "Could not delete the bucket.",
      );
    } finally {
      setDeleting(false);
    }
  }

  return (
    <Modal title={`Delete ${bucket.name}`} onClose={onClose}>
      <div className="space-y-4">
        <p className="text-sm">
          {bucket.objectCount > 0 ? (
            <>
              This bucket holds{" "}
              <span className="font-medium">{bucket.objectCount.toLocaleString()} objects</span>. Empty it
              before deleting.
            </>
          ) : (
            <>This bucket is empty. Deleting it cannot be undone.</>
          )}
        </p>
        {/* Typing the name is a deliberate act; a plain "are you sure" gets
            clicked through without reading. */}
        <ConfirmByName name={bucket.name} typed={typed} onTyped={setTyped} />
        {error && <ErrorNotice message={error} />}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="danger" onClick={remove} disabled={deleting || typed !== bucket.name}>
            {deleting ? "Deleting…" : "Delete bucket"}
          </Button>
        </div>
      </div>
    </Modal>
  );
}
