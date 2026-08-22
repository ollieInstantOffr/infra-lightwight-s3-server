import { useState } from "react";
import { api } from "../lib/api";
import { formatDate } from "../lib/format";
import { Button, CopyButton, ErrorNotice, Field, InfoNotice, Modal, Select } from "./ui";

// A share link is a presigned S3 URL: it works from anywhere, without the
// console, until it expires.

const durations = [
  { value: "3600", label: "1 hour" },
  { value: "86400", label: "24 hours" },
  { value: "604800", label: "7 days (maximum)" },
] as const;

export function ShareModal({
  bucket,
  objectKey,
  onClose,
}: {
  bucket: string;
  objectKey: string;
  onClose: () => void;
}) {
  const [seconds, setSeconds] = useState<string>("86400");
  const [link, setLink] = useState<string | null>(null);
  const [expiresAt, setExpiresAt] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  async function create() {
    setCreating(true);
    setError(null);
    try {
      const result = await api.post<{ url: string; expiresAt: string }>(
        `/api/buckets/${encodeURIComponent(bucket)}/share`,
        { key: objectKey, expiresSeconds: Number(seconds) },
      );
      setLink(result.url);
      setExpiresAt(result.expiresAt);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not create a share link.");
    } finally {
      setCreating(false);
    }
  }

  return (
    <Modal title="Share link" subtitle={objectKey} onClose={onClose}>
      <div className="space-y-[16px]">
        <Field label="Expires after">
          <Select
            value={seconds}
            onChange={setSeconds}
            options={durations.map((duration) => ({ value: duration.value, label: duration.label }))}
            ariaLabel="Link lifetime"
          />
        </Field>

        <InfoNotice>
          Anyone with the link can download this object until it expires — no sign-in required. The
          link is signed with an access key, so revoking that key also revokes every link signed with
          it. That is the only way to withdraw one already sent.
        </InfoNotice>

        {error && <ErrorNotice message={error} />}

        {link && (
          <div>
            <p className="mb-[6px] text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-heading">
              Link · expires {formatDate(expiresAt)}
            </p>
            <textarea
              readOnly
              value={link}
              rows={4}
              onFocus={(event) => event.currentTarget.select()}
              className="w-full rounded-[12px] border border-line bg-inset p-[12px] font-mono text-[11px] leading-[1.6] outline-none"
            />
            <div className="mt-[8px]">
              <CopyButton text={link}>Copy link</CopyButton>
            </div>
          </div>
        )}

        <div className="flex justify-end gap-[8px]">
          <Button onClick={onClose}>Close</Button>
          <Button variant="primary" onClick={create} disabled={creating}>
            {creating ? "Creating…" : link ? "Create another" : "Create link"}
          </Button>
        </div>
      </div>
    </Modal>
  );
}
