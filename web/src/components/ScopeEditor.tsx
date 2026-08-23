import { ALL_PERMISSIONS, type AccessScope, type Permission, type ScopeRule } from "../lib/api";
import { Button, Field, InfoNotice, Select, TextInput, Toggle } from "./ui";

/**
 * Choosing what an access key may do.
 *
 * The design goal is that creating a narrow key is not harder than creating a
 * wide one. A scope offered as an advanced option that people skip is a scope
 * nobody has, so this sits inline in the create flow rather than behind a
 * disclosure, and every key visibly declares which it is.
 */
export function ScopeEditor({
  scope,
  onChange,
  buckets,
}: {
  scope: AccessScope;
  onChange: (scope: AccessScope) => void;
  buckets: string[];
}) {
  function setRule(index: number, rule: ScopeRule) {
    onChange({ ...scope, rules: scope.rules.map((existing, i) => (i === index ? rule : existing)) });
  }

  function addRule() {
    onChange({
      ...scope,
      rules: [...scope.rules, { bucket: buckets[0] ?? "", prefix: "", permissions: ["read"] }],
    });
  }

  function removeRule(index: number) {
    onChange({ ...scope, rules: scope.rules.filter((_, i) => i !== index) });
  }

  return (
    <div className="space-y-[14px]">
      <Toggle
        checked={!scope.unrestricted}
        onChange={(limited) => onChange({ ...scope, unrestricted: !limited })}
        label="Limit this key to certain buckets"
        description="Off means the key can read, write and delete everything, including buckets created later."
      />

      {scope.unrestricted ? (
        <InfoNotice tone="warn">
          This key will have full access to every bucket. Anyone holding it can delete all of your
          objects. Limit it unless it is for your own administration.
        </InfoNotice>
      ) : (
        <div className="space-y-[12px]">
          {scope.rules.length === 0 && (
            <InfoNotice tone="warn">
              No rules yet, so this key can do nothing at all. That is a valid way to park a key,
              but it will not work until you add one.
            </InfoNotice>
          )}

          {scope.rules.map((rule, index) => (
            <div key={index} className="rounded-[11px] border border-line bg-well p-[13px]">
              <div className="flex items-end gap-[10px]">
                <div className="min-w-0 flex-1">
                  <Field label="Bucket">
                    {buckets.length > 0 ? (
                      <Select
                        value={rule.bucket}
                        ariaLabel={`Bucket for rule ${index + 1}`}
                        onChange={(bucket) => setRule(index, { ...rule, bucket })}
                        options={buckets.map((name) => ({ value: name, label: name }))}
                      />
                    ) : (
                      <TextInput
                        value={rule.bucket}
                        onChange={(bucket) => setRule(index, { ...rule, bucket })}
                        placeholder="bucket-name"
                      />
                    )}
                  </Field>
                </div>
                <div className="min-w-0 flex-1">
                  {/* The hint lives under the whole row rather than inside
                      this field, so both fields stay the same height and the
                      two inputs line up. */}
                  <Field label="Prefix">
                    <TextInput
                      value={rule.prefix}
                      onChange={(prefix) => setRule(index, { ...rule, prefix })}
                      placeholder="tenant-a/"
                    />
                  </Field>
                </div>
                <Button onClick={() => removeRule(index)}>Remove</Button>
              </div>

              <div className="mt-[10px] flex flex-wrap gap-[14px]">
                {ALL_PERMISSIONS.map((permission) => (
                  <PermissionCheck
                    key={permission}
                    permission={permission}
                    checked={rule.permissions.includes(permission)}
                    onChange={(checked) =>
                      setRule(index, {
                        ...rule,
                        permissions: checked
                          ? [...rule.permissions, permission]
                          : rule.permissions.filter((p) => p !== permission),
                      })
                    }
                  />
                ))}
              </div>

              <p className="mt-[9px] mb-0 text-[11.5px] leading-[1.5] text-ink-faint">
                {rule.prefix === ""
                  ? "Leave the prefix empty for the whole bucket."
                  : "Limited to a prefix, so this key cannot create or delete the bucket itself, and sees only its own prefix when listing."}
              </p>
            </div>
          ))}

          <Button onClick={addRule}>Add a bucket</Button>
        </div>
      )}
    </div>
  );
}

/** What each permission actually covers, in the words someone granting it would use. */
const permissionHint: Record<Permission, string> = {
  read: "Download objects and list the bucket",
  write: "Upload and overwrite objects",
  delete: "Remove objects",
};

function PermissionCheck({
  permission,
  checked,
  onChange,
}: {
  permission: Permission;
  checked: boolean;
  onChange: (checked: boolean) => void;
}) {
  return (
    <label className="flex cursor-pointer items-start gap-[7px]">
      <input
        type="checkbox"
        checked={checked}
        onChange={(event) => onChange(event.target.checked)}
        className="mt-[3px] size-[15px] flex-none accent-accent"
      />
      <span>
        <span className="block text-[12.5px] font-medium capitalize">{permission}</span>
        <span className="block text-[11px] text-ink-faint">{permissionHint[permission]}</span>
      </span>
    </label>
  );
}

/** The empty scope a new key starts from: unrestricted, as before scoping existed. */
export function unrestrictedScope(): AccessScope {
  return { unrestricted: true, rules: [], summary: "unrestricted" };
}
