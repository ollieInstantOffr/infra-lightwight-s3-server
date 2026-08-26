import { useState } from "react";
import { api, type SetupState } from "../lib/api";
import { useApi } from "../lib/useApi";
import { Button, Card, ErrorNotice, Field, InfoNotice, Spinner, TextInput } from "../components/ui";
import { Logo } from "../components/Logo";

export function SignInPage({ onSignedIn }: { onSignedIn: () => void }) {
  const { data: setup, loading } = useApi<SetupState>("/api/setup");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The bootstrap administrator exists but has no password on a fresh
  // deployment, and on the first start after moving off magic links. Showing a
  // form that would reject the only address that should work — with no
  // explanation — is the worst possible first impression, so the screen shows
  // the one command that fixes it instead.
  const needsFirstPassword = setup !== null && !setup.adminHasPassword;

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      await api.post("/api/auth/login", { email, password });
      onSignedIn();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not sign in.");
      // Kept, so a mistyped password does not also mean retyping the address.
      setPassword("");
    } finally {
      setSubmitting(false);
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
          <Logo size={30} />
          <span className="text-[16px] font-semibold tracking-[-0.01em]">Pail</span>
        </div>

        <Card className="p-[26px]">
          {needsFirstPassword && setup ? (
            <FirstRun setup={setup} />
          ) : (
            <>
              <h1 className="m-0 mb-[7px] text-[23px] font-semibold tracking-[-0.02em]">Sign in</h1>
              <p className="m-0 text-[13px] text-ink-muted">
                Use the email address and password for this console.
              </p>

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
                <Field label="Password">
                  <TextInput
                    type="password"
                    name="password"
                    value={password}
                    onChange={setPassword}
                    required
                  />
                </Field>
                {error && <ErrorNotice message={error} />}
                <Button
                  type="submit"
                  variant="primary"
                  disabled={submitting || email.trim() === "" || password === ""}
                >
                  {submitting ? "Signing in…" : "Sign in"}
                </Button>
              </form>

              <p className="mt-[18px] m-0 text-[12px] leading-[1.6] text-ink-faint">
                Forgotten it? There is no reset email. An administrator can set a new one, or it can
                be reset on the server with{" "}
                <span className="font-mono text-[11.5px]">s3d user set-password</span>.
              </p>
            </>
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

function FirstRun({ setup }: { setup: SetupState }) {
  return (
    <>
      <h1 className="m-0 mb-[5px] text-[20px] font-semibold tracking-[-0.02em]">
        Set the first password
      </h1>
      <p className="m-0 text-[13px] leading-[1.6] text-ink-muted">
        The administrator account exists but has no password, so nobody can sign in yet. Set one on
        the server, then come back here.
      </p>

      <div className="mt-[16px] rounded-[12px] border border-line bg-inset px-[14px] py-[12px]">
        <p className="m-0 mb-[3px] text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-heading">
          Bootstrap administrator
        </p>
        <p className="m-0 font-mono text-[13px]">{setup.adminEmail}</p>
      </div>

      <div className="mt-[12px] rounded-[12px] border border-line bg-inset px-[14px] py-[12px]">
        <p className="m-0 mb-[5px] text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-heading">
          Run this on the server
        </p>
        <code className="block break-all font-mono text-[12px] leading-[1.6]">
          docker compose exec s3d s3d user set-password {setup.adminEmail}
        </code>
      </div>

      <div className="mt-[14px]">
        <InfoNotice tone="warn">
          It asks for the password twice and does not echo it, so it stays out of your shell
          history.
        </InfoNotice>
      </div>

      <div className="mt-[16px]">
        <Button variant="secondary" onClick={() => window.location.reload()}>
          I have set it
        </Button>
      </div>
    </>
  );
}
