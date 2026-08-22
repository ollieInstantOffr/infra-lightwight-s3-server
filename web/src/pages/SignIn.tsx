import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api } from "../lib/api";
import { Button, Card, ErrorNotice, Field, TextInput } from "../components/ui";

// The reasons the callback can redirect back with. Each is turned into a
// sentence that says what to do next, since "error=expired" on its own is
// no help to the person reading it.
const reasons: Record<string, string> = {
  expired: "That sign-in link has expired or was already used. Request a new one below.",
  missing: "That link was incomplete. Request a new one below.",
  "not-invited": "That address does not have access to this console. Ask an administrator for an invitation.",
};

export function SignInPage() {
  const [params] = useSearchParams();
  const [email, setEmail] = useState("");
  const [sent, setSent] = useState(false);
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reason = params.get("error");
  const reasonMessage = reason ? (reasons[reason] ?? "That sign-in link did not work. Request a new one below.") : null;

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

  return (
    <div className="flex min-h-full items-center justify-center px-4 py-16">
      <Card className="w-full max-w-sm p-6">
        <h1 className="text-lg font-semibold tracking-tight">Sign in</h1>
        <p className="mt-1 text-sm text-ink-muted">
          We will email you a link. There is no password.
        </p>

        {reasonMessage && (
          <div className="mt-4">
            <ErrorNotice message={reasonMessage} />
          </div>
        )}

        {sent ? (
          // Deliberately says "if that address can sign in" rather than
          // confirming the account exists — the server is careful not to
          // reveal that, and the interface must not undo it.
          <div className="mt-6 space-y-3">
            <p className="text-sm">
              If <span className="font-medium">{email}</span> can sign in, a link is on its way.
            </p>
            <p className="text-sm text-ink-muted">
              The link expires in 15 minutes and can only be used once.
            </p>
            <Button variant="secondary" onClick={() => setSent(false)}>
              Use a different address
            </Button>
          </div>
        ) : (
          <form className="mt-6 space-y-4" onSubmit={submit}>
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
            <Button type="submit" disabled={sending || email.trim() === ""}>
              {sending ? "Sending…" : "Email me a link"}
            </Button>
          </form>
        )}
      </Card>
    </div>
  );
}
