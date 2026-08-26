import { useEffect, useState } from "react";
import { api, type AlertEmailSettings } from "../lib/api";
import { useApi } from "../lib/useApi";
import {
  Button,
  Card,
  ErrorNotice,
  Field,
  InfoNotice,
  PageHeader,
  Spinner,
  TextInput,
} from "../components/ui";

/**
 * Settings that are not per-bucket. Currently one: where alert email goes.
 *
 * Email used to be part of signing in, configured by environment variable and
 * read once at startup. It now carries alerts only, and lives here because the
 * moment you need to correct a bad API key is the moment alerts have stopped
 * arriving — which is the worst possible time to be editing .env and
 * redeploying.
 */
export function SettingsPage() {
  const { data, loading, error, reload } = useApi<AlertEmailSettings>("/api/settings/alert-email");

  return (
    <>
      <PageHeader
        title="Settings"
        subtitle="How this deployment notifies you when something needs attention."
      />
      {loading && <Spinner label="Loading settings" />}
      {error && <ErrorNotice message={error} />}
      {data && <AlertEmailCard settings={data} onSaved={reload} />}
    </>
  );
}

function AlertEmailCard({
  settings,
  onSaved,
}: {
  settings: AlertEmailSettings;
  onSaved: () => void;
}) {
  const [enabled, setEnabled] = useState(settings.enabled);
  const [from, setFrom] = useState(settings.from);
  // Always starts empty. The server never sends the stored key back, and an
  // empty value on save means "keep the one you have" — so the key can be left
  // alone while the other fields change.
  const [apiKey, setApiKey] = useState("");
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  useEffect(() => {
    setEnabled(settings.enabled);
    setFrom(settings.from);
  }, [settings]);

  async function save(event: React.FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError(null);
    setNotice(null);
    try {
      await api.put("/api/settings/alert-email", { enabled, from, apiKey });
      setApiKey("");
      setNotice("Saved.");
      onSaved();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not save.");
    } finally {
      setSaving(false);
    }
  }

  async function sendTest() {
    setTesting(true);
    setError(null);
    setNotice(null);
    try {
      const result = await api.post<{ message: string }>("/api/settings/alert-email/test");
      setNotice(result.message);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "The test message could not be sent.");
    } finally {
      setTesting(false);
    }
  }

  return (
    <Card className="p-[22px]">
      <h2 className="m-0 mb-[5px] text-[16px] font-semibold tracking-[-0.01em]">Alert email</h2>
      <p className="m-0 text-[12.5px] leading-[1.6] text-ink-muted">
        Alerts are delivered through Resend to every administrator. Without it, alerts still appear
        in the console — they just do not reach you when you are not looking at it.
      </p>

      <form className="mt-[20px] space-y-[16px]" onSubmit={save}>
        <label className="flex items-start gap-[10px]">
          <input
            type="checkbox"
            className="mt-[3px]"
            checked={enabled}
            onChange={(event) => setEnabled(event.target.checked)}
          />
          <span className="text-[13px] leading-[1.5]">
            Send alert notifications by email
            <span className="block text-[12px] text-ink-faint">
              Needs an API key and a from-address that Resend has verified.
            </span>
          </span>
        </label>

        <Field
          label="From address"
          hint="Must be on a domain verified in your Resend account, or every send is rejected."
        >
          <TextInput
            type="email"
            name="from"
            value={from}
            onChange={setFrom}
            placeholder="alerts@example.com"
          />
        </Field>

        <Field
          label="Resend API key"
          hint={
            settings.hasApiKey
              ? "A key is stored. Leave this blank to keep it, or paste a new one to replace it."
              : "No key is stored yet."
          }
        >
          <TextInput
            type="password"
            name="apiKey"
            value={apiKey}
            onChange={setApiKey}
            placeholder={settings.hasApiKey ? "••••••••••••••••" : "re_..."}
          />
        </Field>

        {error && <ErrorNotice message={error} />}
        {notice && <InfoNotice tone="accent">{notice}</InfoNotice>}

        <div className="flex flex-wrap gap-[10px]">
          <Button type="submit" variant="primary" disabled={saving}>
            {saving ? "Saving…" : "Save"}
          </Button>
          {/* A mail configuration only exercised when something is already
              broken is a mail configuration nobody trusts. */}
          <Button
            variant="secondary"
            onClick={sendTest}
            disabled={testing || saving || !settings.hasApiKey}
          >
            {testing ? "Sending…" : "Send a test to me"}
          </Button>
        </div>
      </form>

      <p className="mt-[16px] m-0 text-[11.5px] leading-[1.6] text-ink-faint">
        The key is encrypted before it is stored and is never sent back to this page. Replacing it
        is the only way to change it.
      </p>
    </Card>
  );
}
