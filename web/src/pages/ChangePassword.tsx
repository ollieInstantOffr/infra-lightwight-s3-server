import { useState } from "react";
import { api } from "../lib/api";
import { useSession } from "../lib/session";
import { Button, Card, ErrorNotice, Field, InfoNotice, TextInput } from "../components/ui";
import { Logo } from "../components/Logo";

/**
 * Shown instead of the app when the signed-in user's password was chosen by
 * somebody else.
 *
 * It is not a route the user can navigate away from: the server refuses every
 * other endpoint while the flag is set, so a screen they could dismiss would
 * only produce a console where nothing works and no explanation is offered.
 */
export function ForcedPasswordChangePage() {
  const { refresh, signOut } = useSession();

  return (
    <div className="flex min-h-full items-center justify-center px-4 py-[8vh]">
      <div className="w-full max-w-[420px]">
        <div className="mb-[26px] flex items-center gap-[10px]">
          <Logo size={30} />
          <span className="text-[16px] font-semibold tracking-[-0.01em]">Pail</span>
        </div>

        <Card className="p-[26px]">
          <h1 className="m-0 mb-[7px] text-[23px] font-semibold tracking-[-0.02em]">
            Choose a new password
          </h1>
          <p className="m-0 text-[13px] leading-[1.6] text-ink-muted">
            Your current password was set by an administrator, so they know it too. Pick one only
            you know before continuing.
          </p>

          <div className="mt-[20px]">
            <ChangePasswordForm
              submitLabel="Set my password"
              onDone={() => {
                // The flag is cleared server-side; re-reading the session is
                // what lets the app render normally again.
                void refresh();
              }}
            />
          </div>

          <div className="mt-[18px] border-t border-line pt-[14px]">
            <button
              className="text-[12px] text-ink-faint underline-offset-2 hover:underline"
              onClick={() => void signOut()}
            >
              Sign out instead
            </button>
          </div>
        </Card>
      </div>
    </div>
  );
}

/**
 * The password change form, shared between the forced screen and the account
 * page so the rules and the error handling cannot drift apart.
 */
export function ChangePasswordForm({
  submitLabel = "Change password",
  onDone,
}: {
  submitLabel?: string;
  onDone?: () => void;
}) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  // Checked here as well as on the server, because a mismatch is the one error
  // worth catching before a round trip: the user cannot see what they typed.
  const mismatch = confirm !== "" && next !== confirm;

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (next !== confirm) {
      setError("The two new passwords do not match.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await api.post("/api/account/password", { currentPassword: current, newPassword: next });
      setDone(true);
      setCurrent("");
      setNext("");
      setConfirm("");
      onDone?.();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not change the password.");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form className="space-y-[16px]" onSubmit={submit}>
      <Field label="Current password">
        <TextInput
          type="password"
          name="currentPassword"
          value={current}
          onChange={setCurrent}
          required
          autoFocus
        />
      </Field>
      <Field label="New password" hint="At least 12 characters. Length is the only rule.">
        <TextInput type="password" name="newPassword" value={next} onChange={setNext} required />
      </Field>
      <Field label="Repeat new password">
        <TextInput
          type="password"
          name="confirmPassword"
          value={confirm}
          onChange={setConfirm}
          required
        />
      </Field>

      {mismatch && <ErrorNotice message="The two new passwords do not match." />}
      {error && !mismatch && <ErrorNotice message={error} />}
      {done && (
        <InfoNotice tone="accent">
          Password changed. Any other sessions have been signed out.
        </InfoNotice>
      )}

      <Button
        type="submit"
        variant="primary"
        disabled={submitting || current === "" || next === "" || next !== confirm}
      >
        {submitting ? "Saving…" : submitLabel}
      </Button>
    </form>
  );
}
