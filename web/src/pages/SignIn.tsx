import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api, type SetupState } from "../lib/api";
import { useApi } from "../lib/useApi";
import { Button, Card, ErrorNotice, Field, InfoNotice, Spinner, TextInput } from "../components/ui";

// The reasons the callback can send someone back with. Each becomes a sentence
// saying what to do next; "error=expired" on its own helps nobody.
const reasons: Record<string, string> = {
  expired: "That sign-in link has expired or was already used. Request a new one below.",
  missing: "That link was incomplete. Request a new one below.",
  "not-invited":
    "That address does not have access to this console. Ask an administrator for an invitation.",
};

export function SignInPage() {
  const [params] = useSearchParams();
  const { data: setup, loading } = useApi<SetupState>("/api/setup");
  const [email, setEmail] = useState("");
  const [sent, setSent] = useState(false);
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reason = params.get("error");
  const reasonMessage = reason
    ? (reasons[reason] ?? "That sign-in link did not work. Request a new one below.")
    : null;

  // A fresh install shows what to do rather than a bare form: without this,
  // the first thing an operator meets is a login box with no indication of
  // which address will work.
  const firstRun = setup !== null && !setup.configured;

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setSending(true);
    setError(null);
    try {
      await api.post("/api/auth/magic-link", { email });
      setSent(true);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not request a sign-in link.");
    } finally {
      setSending(false);
    }
  }

  if (loading) {
    return (
      <div className="flex min-h-full items-center justify-center">
        <Spinner label="Loading" />
      </div>
    );
  }

  return (
    <div className="flex min-h-full items-center justify-center px-4 py-[8vh]">
      <div className="w-full max-w-[400px]">
        <div className="mb-[26px] flex items-center gap-[10px]">
          <div className="flex size-[30px] items-center justify-center rounded-[9px] bg-accent font-mono text-[13px] font-semibold text-on-accent">
            P
          </div>
          <span className="text-[16px] font-semibold tracking-[-0.01em]">Pail</span>
        </div>

        <Card className="p-[26px]">
          {firstRun ? (
            <FirstRun setup={setup} onUse={(address) => setEmail(address)} />
          ) : (
            <>
              <h1 className="m-0 mb-[7px] text-[23px] font-semibold tracking-[-0.02em]">Sign in</h1>
              <p className="m-0 text-[13px] text-ink-muted">
                We will email you a link. There is no password.
              </p>
            </>
          )}

          {reasonMessage && (
            <div className="mt-[18px]">
              <ErrorNotice message={reasonMessage} />
            </div>
          )}

          {sent ? (
            <div className="mt-[22px] space-y-[12px]">
              <h2 className="m-0 text-[21px] font-semibold tracking-[-0.02em]">Check your inbox</h2>
              {/* Deliberately conditional: the server is careful not to confirm
                  whether the address exists, and the interface must not undo
                  that by phrasing it as a certainty. */}
              <p className="m-0 text-[13px] leading-[1.6]">
                If <span className="font-medium">{email}</span> can sign in, a link is on its way.
              </p>
              <p className="m-0 text-[12.5px] text-ink-muted">
                It expires in 15 minutes and can only be used once.
              </p>
              {setup && !setup.emailConfigured && (
                <InfoNotice tone="warn">
                  No email provider is configured, so the link was written to the server log instead
                  of sent. Run <span className="font-mono">docker compose logs s3d</span> to find it.
                </InfoNotice>
              )}
              <Button variant="secondary" onClick={() => setSent(false)}>
                Use a different address
              </Button>
            </div>
          ) : (
            <form className="mt-[22px] space-y-[16px]" onSubmit={submit}>
              <Field label="Email address">
                <TextInput
                  type="email"
                  name="email"
                  value={email}
                  onChange={setEmail}
                  placeholder="you@example.com"
                  required
                  autoFocus
                />
              </Field>
              {error && <ErrorNotice message={error} />}
              <Button type="submit" variant="primary" disabled={sending || email.trim() === ""}>
                {sending ? "Sending…" : "Email me a link"}
              </Button>
            </form>
          )}
        </Card>

        <p className="mt-[18px] text-center text-[11.5px] leading-[1.6] text-ink-faint">
          S3-compatible object storage running on your own hardware.
          <br />
          Objects are stored as a single copy, with no replication.
        </p>
      </div>
    </div>
  );
}

function FirstRun({ setup, onUse }: { setup: SetupState; onUse: (email: string) => void }) {
  return (
    <>
      <h1 className="m-0 mb-[5px] text-[20px] font-semibold tracking-[-0.02em]">
        Create the first admin
      </h1>
      <p className="m-0 text-[13px] leading-[1.6] text-ink-muted">
        Nobody has signed in yet. The address in <span className="font-mono text-[12px]">ADMIN_EMAIL</span>{" "}
        is the only one that can, until it invites others.
      </p>
      <div className="mt-[16px] rounded-[12px] border border-line bg-inset px-[14px] py-[12px]">
        <p className="m-0 mb-[3px] text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-heading">
          Bootstrap administrator
        </p>
        <button
          className="font-mono text-[13px] underline-offset-2 hover:underline"
          onClick={() => onUse(setup.adminEmail)}
          title="Use this address"
        >
          {setup.adminEmail}
        </button>
      </div>
    </>
  );
}
