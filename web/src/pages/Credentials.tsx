import { useState } from "react";
import { api, type Credential, type CreatedCredential } from "../lib/api";
import { useApi } from "../lib/useApi";
import { formatDate, formatRelative } from "../lib/format";
import {
  Button,
  Card,
  CodeBlock,
  CopyButton,
  EmptyState,
  ErrorNotice,
  Field,
  InfoNotice,
  KeyValueRow,
  Modal,
  PageHeader,
  RowAction,
  SkeletonLine,
  Tag,
  TableHead,
  TableRow,
  TextInput,
} from "../components/ui";

const columns = "grid-cols-[1.2fr_1.6fr_.9fr_.9fr_auto]";

export function CredentialsPage() {
  const { data, error, loading, reload } = useApi<{ credentials: Credential[] }>("/api/credentials");
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<CreatedCredential | null>(null);
  const [revoking, setRevoking] = useState<Credential | null>(null);

  return (
    <>
      <PageHeader
        title="Access keys"
        subtitle="S3-compatible key pairs. Secrets are shown once, at creation."
        actions={
          <Button variant="primary" onClick={() => setCreating(true)}>
            Create key
          </Button>
        }
      />

      {error && <ErrorNotice message={error} onRetry={reload} />}

      {created && <NewSecret credential={created} onDismiss={() => setCreated(null)} />}

      <Card className="overflow-hidden">
        <TableHead
          columns={["Label", "Access key id", "Created", "Last used", ""]}
          className={columns}
        />

        {loading &&
          Array.from({ length: 3 }, (_, index) => (
            <div key={index} className={`grid ${columns} gap-[10px] border-b border-line-row px-[18px] py-[13px]`}>
              <SkeletonLine width="60%" />
              <SkeletonLine width="80%" faint />
              <SkeletonLine width={70} faint />
              <SkeletonLine width={54} faint />
              <span />
            </div>
          ))}

        {data?.credentials.map((credential) => (
          <TableRow key={credential.accessKeyId} className={columns}>
            <span className="flex min-w-0 items-center gap-[6px]">
              <span className="truncate text-[12.5px] font-medium">
                {credential.description || "Unlabelled"}
              </span>
              {credential.revoked && <Tag tone="danger">revoked</Tag>}
            </span>
            <span className="truncate font-mono text-[12px] text-ink-muted">{credential.accessKeyId}</span>
            <span className="text-[12.5px] text-ink-muted">{formatDate(credential.createdAt)}</span>
            <span className="text-[12.5px] text-ink-muted">{formatRelative(credential.lastUsedAt)}</span>
            <span className="flex justify-end gap-[2px]">
              <CopyButton text={credential.accessKeyId} />
              {!credential.revoked && (
                <RowAction danger onClick={() => setRevoking(credential)}>
                  Revoke
                </RowAction>
              )}
            </span>
          </TableRow>
        ))}

        {data && data.credentials.length === 0 && (
          <EmptyState
            title="No access keys yet"
            hint="The S3 API needs a key pair. Create one to connect aws-cli, boto3 or an SDK."
            action={
              <Button variant="primary" onClick={() => setCreating(true)}>
                Create key
              </Button>
            }
          />
        )}
      </Card>

      {creating && (
        <CreateKeyModal
          onClose={() => setCreating(false)}
          onCreated={(credential) => {
            setCreating(false);
            setCreated(credential);
            reload();
          }}
        />
      )}

      {revoking && (
        <RevokeModal
          credential={revoking}
          onClose={() => setRevoking(null)}
          onRevoked={() => {
            setRevoking(null);
            reload();
          }}
        />
      )}
    </>
  );
}

/**
 * The one-time secret panel.
 *
 * Only shown immediately after creation, and deliberately hard to dismiss by
 * accident: the secret is not recoverable, and a user who clicks past this has
 * to delete the key and make another.
 */
function NewSecret({
  credential,
  onDismiss,
}: {
  credential: CreatedCredential;
  onDismiss: () => void;
}) {
  const [acknowledged, setAcknowledged] = useState(false);
  const [snippet, setSnippet] = useState<string>("awscli");

  const snippetLabels: Record<string, string> = {
    env: "Environment",
    awscli: "AWS CLI",
    boto3: "Python (boto3)",
    go: "Go (SDK v2)",
    nodejs: "Node (SDK v3)",
  };

  return (
    <Card className="mb-[16px] border-accent/30 bg-accent-soft p-[18px]">
      <div className="flex items-start gap-[12px]">
        <span className="mt-[2px] flex size-[22px] flex-none items-center justify-center rounded-full bg-accent text-[12px] font-semibold text-on-accent">
          ↓
        </span>
        <div className="min-w-0 flex-1">
          <p className="m-0 text-[13.5px] font-semibold text-accent-deep">
            Save your secret key — this is the only time it is shown.
          </p>
          <p className="m-0 mt-[3px] text-[12.5px] text-accent-deep/80">{credential.warning}</p>

          <div className="mt-[14px] rounded-[12px] bg-card px-[14px] py-[4px]">
            <KeyValueRow label="Access key id" value={credential.accessKeyId} />
            <KeyValueRow label="Secret key" value={credential.secretAccessKey} masked />
            <KeyValueRow label="Endpoint" value={credential.endpoint} />
            <KeyValueRow label="Region" value={credential.region} />
          </div>

          <div className="mt-[14px]">
            <div className="mb-[8px] flex flex-wrap gap-[4px]">
              {Object.keys(snippetLabels).map((key) => (
                <button
                  key={key}
                  onClick={() => setSnippet(key)}
                  className={`rounded-[7px] px-[9px] py-[5px] text-[11.5px] font-medium ${
                    snippet === key ? "bg-accent text-on-accent" : "bg-card text-ink-muted hover:text-ink"
                  }`}
                >
                  {snippetLabels[key]}
                </button>
              ))}
            </div>
            <CodeBlock text={credential.snippets[snippet] ?? ""} />
          </div>

          <label className="mt-[14px] flex items-center gap-[8px] text-[12.5px]">
            <input
              type="checkbox"
              checked={acknowledged}
              onChange={(event) => setAcknowledged(event.target.checked)}
            />
            I have saved the secret key somewhere safe.
          </label>

          <div className="mt-[10px]">
            <Button variant="primary" disabled={!acknowledged} onClick={onDismiss}>
              Done
            </Button>
          </div>
        </div>
      </div>
    </Card>
  );
}

function CreateKeyModal({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (credential: CreatedCredential) => void;
}) {
  const [description, setDescription] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError(null);
    try {
      onCreated(await api.post<CreatedCredential>("/api/credentials", { description }));
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not create the key.");
      setSaving(false);
    }
  }

  return (
    <Modal title="Create an access key" onClose={onClose}>
      <form className="space-y-[16px]" onSubmit={submit}>
        <Field
          label="Label"
          hint="What will use this key. It is the only way to tell keys apart later."
        >
          <TextInput value={description} onChange={setDescription} placeholder="ci-deploy" autoFocus />
        </Field>
        <InfoNotice>
          The secret is shown once and stored encrypted. If it is lost, revoke the key and create
          another — there is no way to recover it.
        </InfoNotice>
        {error && <ErrorNotice message={error} />}
        <div className="flex justify-end gap-[8px]">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={saving}>
            {saving ? "Creating…" : "Create key"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

function RevokeModal({
  credential,
  onClose,
  onRevoked,
}: {
  credential: Credential;
  onClose: () => void;
  onRevoked: () => void;
}) {
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function revoke() {
    setBusy(true);
    try {
      await api.delete(`/api/credentials/${encodeURIComponent(credential.accessKeyId)}`);
      onRevoked();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not revoke the key.");
      setBusy(false);
    }
  }

  return (
    <Modal title="Revoke access key" subtitle={credential.accessKeyId} onClose={onClose}>
      <div className="space-y-[16px]">
        <p className="m-0 text-[13px] leading-[1.6]">
          This takes effect on the very next request. Anything using it — including any share links
          signed with it — stops working immediately.
        </p>
        {error && <ErrorNotice message={error} />}
        <div className="flex justify-end gap-[8px]">
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" onClick={revoke} disabled={busy}>
            {busy ? "Revoking…" : "Revoke key"}
          </Button>
        </div>
      </div>
    </Modal>
  );
}
