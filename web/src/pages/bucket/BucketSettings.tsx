import { useEffect, useState } from "react";
import { api, type BucketSettings, type CorsRule, type LifecycleRule } from "../../lib/api";
import { formatBytes } from "../../lib/format";
import {
  Button,
  Card,
  ErrorNotice,
  Field,
  InfoNotice,
  RowAction,
  Select,
  Spinner,
  TextInput,
  Toggle,
} from "../../components/ui";

// Access, CORS and lifecycle. Each of these can quietly do something the
// operator did not intend, so each says what it will actually cause.

export function BucketSettingsTab({ bucket, onSaved }: { bucket: string; onSaved: () => void }) {
  const [settings, setSettings] = useState<BucketSettings | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    api
      .get<BucketSettings>(`/api/buckets/${encodeURIComponent(bucket)}/settings`)
      .then(setSettings)
      .catch((caught: unknown) =>
        setError(caught instanceof Error ? caught.message : "Could not load settings."),
      );
  }, [bucket]);

  async function save() {
    if (!settings) return;
    setSaving(true);
    setError(null);
    try {
      await api.put(`/api/buckets/${encodeURIComponent(bucket)}/settings`, {
        publicRead: settings.publicRead,
        versioning: settings.versioning,
        corsRules: settings.corsRules,
        lifecycleRules: settings.lifecycleRules,
      });
      setSaved(true);
      window.setTimeout(() => setSaved(false), 2000);
      onSaved();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not save settings.");
    } finally {
      setSaving(false);
    }
  }

  if (error && !settings) return <ErrorNotice message={error} />;
  if (!settings) return <Spinner label="Loading settings" />;

  const update = (patch: Partial<BucketSettings>) => setSettings({ ...settings, ...patch });

  return (
    <div className="space-y-[16px]">
      <Card padded>
        <h2 className="m-0 mb-[4px] text-[14px] font-semibold">Access</h2>
        <p className="m-0 mb-[16px] text-[12.5px] text-ink-muted">
          Who can read this bucket without an access key.
        </p>

        <Toggle
          checked={settings.publicRead}
          onChange={(publicRead) => update({ publicRead })}
          label="Public read"
          description="Anyone who knows an object's URL can download it, with no credentials. Anonymous writes are never permitted, whatever this is set to."
        />

        {settings.publicRead && (
          <div className="mt-[14px]">
            <InfoNotice tone="warn">
              Every object in this bucket becomes readable by anyone with its URL. Object keys are
              not secret — a listing is not required to guess one.
            </InfoNotice>
          </div>
        )}
      </Card>

      <Card padded>
        <h2 className="m-0 mb-[4px] text-[14px] font-semibold">Versioning</h2>
        <p className="m-0 mb-[16px] text-[12.5px] text-ink-muted">
          Keep previous states of an object instead of discarding them.
        </p>

        <Toggle
          checked={settings.versioning}
          onChange={(versioning) => update({ versioning })}
          label="Keep version history"
          description="Overwrites and deletes become history that can be restored. Each version still has exactly one copy of its bytes — this is history, not redundancy."
        />

        {settings.versionCount > 0 && (
          <div className="mt-[14px] rounded-[12px] border border-line bg-inset px-[14px] py-[12px] text-[12.5px]">
            <p className="m-0">
              <span className="font-semibold">{formatBytes(settings.versionedBytes)}</span> is held by{" "}
              {settings.versionCount.toLocaleString()} superseded{" "}
              {settings.versionCount === 1 ? "version" : "versions"}.
            </p>
            <p className="m-0 mt-[4px] text-ink-muted">
              This space is not reclaimed until those versions are purged, which is done from the
              Versions tab.
            </p>
          </div>
        )}
      </Card>

      <CorsEditor rules={settings.corsRules} onChange={(corsRules) => update({ corsRules })} />
      <LifecycleEditor
        rules={settings.lifecycleRules}
        onChange={(lifecycleRules) => update({ lifecycleRules })}
      />

      {error && <ErrorNotice message={error} />}

      <div className="flex items-center gap-[10px]">
        <Button variant="primary" onClick={save} disabled={saving}>
          {saving ? "Saving…" : "Save settings"}
        </Button>
        {saved && <span className="text-[12.5px] text-ok">Saved.</span>}
      </div>
    </div>
  );
}

function CorsEditor({ rules, onChange }: { rules: CorsRule[]; onChange: (rules: CorsRule[]) => void }) {
  return (
    <Card padded>
      <div className="mb-[4px] flex items-center justify-between">
        <h2 className="m-0 text-[14px] font-semibold">CORS</h2>
        <Button
          onClick={() =>
            onChange([
              ...rules,
              { allowedOrigins: [""], allowedMethods: ["GET"], allowedHeaders: ["*"], maxAgeSeconds: 3600 },
            ])
          }
        >
          Add rule
        </Button>
      </div>
      <p className="m-0 mb-[16px] text-[12.5px] text-ink-muted">
        Which websites may read this bucket from a browser. Without a matching rule, the browser
        blocks the request before it reaches this server.
      </p>

      {rules.length === 0 && (
        <p className="m-0 text-[12.5px] text-ink-faint">
          No rules. Browser requests from other origins will be refused.
        </p>
      )}

      <div className="space-y-[12px]">
        {rules.map((rule, index) => (
          <div key={index} className="rounded-[12px] border border-line bg-inset p-[13px]">
            <div className="mb-[10px] flex items-center justify-between">
              <span className="text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-heading">
                Rule {index + 1}
              </span>
              <RowAction danger onClick={() => onChange(rules.filter((_, i) => i !== index))}>
                Remove
              </RowAction>
            </div>
            <div className="grid gap-[12px] sm:grid-cols-2">
              <Field label="Allowed origins" hint="One per line. * allows any origin.">
                <textarea
                  value={rule.allowedOrigins.join("\n")}
                  onChange={(event) =>
                    onChange(
                      rules.map((current, i) =>
                        i === index
                          ? { ...current, allowedOrigins: event.target.value.split("\n").map((line) => line.trim()) }
                          : current,
                      ),
                    )
                  }
                  rows={3}
                  className="w-full rounded-[10px] border border-line-input bg-card px-[13px] py-[10px] font-mono text-[12px] outline-none focus:border-accent"
                  placeholder="https://app.example.com"
                />
              </Field>
              <Field label="Allowed methods">
                <div className="flex flex-wrap gap-[6px]">
                  {["GET", "HEAD", "PUT", "POST", "DELETE"].map((method) => {
                    const on = rule.allowedMethods.includes(method);
                    return (
                      <button
                        key={method}
                        type="button"
                        onClick={() =>
                          onChange(
                            rules.map((current, i) =>
                              i === index
                                ? {
                                    ...current,
                                    allowedMethods: on
                                      ? current.allowedMethods.filter((m) => m !== method)
                                      : [...current.allowedMethods, method],
                                  }
                                : current,
                            ),
                          )
                        }
                        className={`rounded-[7px] px-[9px] py-[5px] font-mono text-[11px] font-semibold ${
                          on ? "bg-accent text-on-accent" : "border border-line bg-card text-ink-muted"
                        }`}
                      >
                        {method}
                      </button>
                    );
                  })}
                </div>
              </Field>
            </div>
          </div>
        ))}
      </div>
    </Card>
  );
}

function LifecycleEditor({
  rules,
  onChange,
}: {
  rules: LifecycleRule[];
  onChange: (rules: LifecycleRule[]) => void;
}) {
  return (
    <Card padded>
      <div className="mb-[4px] flex items-center justify-between">
        <h2 className="m-0 text-[14px] font-semibold">Lifecycle</h2>
        <Button
          onClick={() =>
            onChange([...rules, { id: `rule-${rules.length + 1}`, prefix: "", expireDays: 30, enabled: false }])
          }
        >
          Add rule
        </Button>
      </div>
      <p className="m-0 mb-[16px] text-[12.5px] text-ink-muted">
        Delete objects automatically once they reach an age. Deletion goes through the ordinary path,
        so versioning applies if it is on.
      </p>

      {rules.length === 0 && <p className="m-0 text-[12.5px] text-ink-faint">No rules. Nothing expires.</p>}

      <div className="space-y-[12px]">
        {rules.map((rule, index) => {
          const patch = (changes: Partial<LifecycleRule>) =>
            onChange(rules.map((current, i) => (i === index ? { ...current, ...changes } : current)));

          return (
            <div key={index} className="rounded-[12px] border border-line bg-inset p-[13px]">
              <div className="grid items-end gap-[12px] sm:grid-cols-[1fr_1.4fr_auto_auto]">
                <Field label="Name">
                  <TextInput value={rule.id} onChange={(id) => patch({ id })} mono />
                </Field>
                <Field label="Prefix" hint={rule.prefix === "" ? "Empty means the whole bucket." : undefined}>
                  <TextInput value={rule.prefix} onChange={(prefix) => patch({ prefix })} placeholder="logs/" mono />
                </Field>
                <Field label="Expire after">
                  <Select
                    value={String(rule.expireDays)}
                    onChange={(days) => patch({ expireDays: Number(days) })}
                    options={[7, 30, 90, 180, 365].map((days) => ({
                      value: String(days),
                      label: `${days} days`,
                    }))}
                    ariaLabel="Expire after"
                  />
                </Field>
                <div className="flex items-center gap-[8px] pb-[9px]">
                  <Toggle checked={rule.enabled} onChange={(enabled) => patch({ enabled })} label="On" />
                  <RowAction danger onClick={() => onChange(rules.filter((_, i) => i !== index))}>
                    Remove
                  </RowAction>
                </div>
              </div>

              {rule.enabled && rule.prefix === "" && (
                <div className="mt-[10px]">
                  <InfoNotice tone="warn">
                    This rule covers the entire bucket. Every object older than {rule.expireDays} days
                    will be deleted.
                  </InfoNotice>
                </div>
              )}
            </div>
          );
        })}
      </div>
    </Card>
  );
}
