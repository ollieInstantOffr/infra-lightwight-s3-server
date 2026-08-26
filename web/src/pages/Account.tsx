import { useCallback, useEffect, useState } from "react";
import { api, type Session } from "../lib/api";
import { useSession } from "../lib/session";
import { ChangePasswordForm } from "./ChangePassword";
import { formatDate, formatRelative } from "../lib/format";
import {
  Button,
  Card,
  ErrorNotice,
  InfoNotice,
  PageHeader,
  RowAction,
  Spinner,
  Tag,
  TableHead,
  TableRow,
} from "../components/ui";

const columns = "grid-cols-[1.6fr_1fr_1fr_1fr_auto]";

export function AccountPage() {
  const { user } = useSession();
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    api
      .get<{ sessions: Session[] }>("/api/account/sessions")
      .then((result) => setSessions(result.sessions))
      .catch((caught: unknown) =>
        setError(caught instanceof Error ? caught.message : "Could not load your sessions."),
      );
  }, []);

  useEffect(load, [load]);

  async function revoke(session: Session) {
    setBusy(true);
    try {
      await api.delete(`/api/account/sessions/${session.id}`);
      load();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not sign that device out.");
    } finally {
      setBusy(false);
    }
  }

  async function revokeOthers() {
    setBusy(true);
    try {
      await api.post("/api/account/sessions/revoke-others");
      load();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not sign the other devices out.");
    } finally {
      setBusy(false);
    }
  }

  const others = sessions?.filter((session) => !session.current).length ?? 0;

  return (
    <>
      <PageHeader
        title="Your account"
        subtitle={user?.email}
        actions={
          others > 0 ? (
            <Button variant="danger" onClick={revokeOthers} disabled={busy}>
              Sign out everywhere else
            </Button>
          ) : undefined
        }
      />

      {error && <ErrorNotice message={error} />}

      <Card className="mb-[16px] p-[22px]">
        <h2 className="m-0 mb-[5px] text-[16px] font-semibold tracking-[-0.01em]">Password</h2>
        <p className="m-0 mb-[18px] text-[12.5px] leading-[1.6] text-ink-muted">
          Changing it signs out every other device. There is no reset email — if you lose it, an
          administrator can set a new one.
        </p>
        <div className="max-w-[380px]">
          <ChangePasswordForm />
        </div>
      </Card>

      <Card className="mb-[16px] overflow-hidden">
        <div className="flex items-center justify-between border-b border-line-row px-[18px] py-[14px]">
          <span className="text-[13.5px] font-semibold">Signed in on</span>
          <span className="text-[12px] text-ink-faint">
            {sessions ? `${sessions.length} active` : ""}
          </span>
        </div>

        <TableHead columns={["Device", "Address", "Signed in", "Last seen", ""]} className={columns} />

        {sessions === null && <Spinner label="Loading sessions" />}

        {sessions?.map((session) => (
          <TableRow key={session.id} className={columns}>
            <span className="flex min-w-0 items-center gap-[7px]">
              <span className="truncate text-[12.5px]">{session.device}</span>
              {session.current && <Tag tone="accent">this device</Tag>}
            </span>
            <span className="truncate font-mono text-[11.5px] text-ink-muted">{session.ip ?? "—"}</span>
            <span className="text-[12.5px] text-ink-muted">{formatDate(session.createdAt)}</span>
            <span className="text-[12.5px] text-ink-muted">{formatRelative(session.lastSeenAt)}</span>
            <span className="text-right">
              {/* The current session is deliberately not revocable here:
                  signing yourself out from the button you just pressed reads
                  as a bug. Sign out in the sidebar does that. */}
              {!session.current && (
                <RowAction danger onClick={() => void revoke(session)} disabled={busy}>
                  Sign out
                </RowAction>
              )}
            </span>
          </TableRow>
        ))}
      </Card>

      <InfoNotice>
        <p className="m-0 mb-[4px] font-semibold">How sign-in works here</p>
        <p className="m-0">
          There is no password. Each sign-in is a single-use link, and a session lasts up to 30 days
          with 12 hours of inactivity. Signing a device out takes effect on its next request.
        </p>
      </InfoNotice>
    </>
  );
}
