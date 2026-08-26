import { useState } from "react";
import { api, type IssuedPassword, type User } from "../lib/api";
import { useApi } from "../lib/useApi";
import { useSession } from "../lib/session";
import { formatDate, formatRelative } from "../lib/format";
import {
  Button,
  Card,
  ErrorNotice,
  Field,
  InfoNotice,
  Modal,
  PageHeader,
  RowAction,
  Select,
  SkeletonLine,
  Tag,
  TableHead,
  TableRow,
  TextInput,
} from "../components/ui";

const memberColumns = "grid-cols-[2fr_.8fr_1fr_1fr_auto]";

export function UsersPage() {
  const { user } = useSession();
  const users = useApi<{ users: User[] }>("/api/users");
  const [creating, setCreating] = useState(false);
  const [removing, setRemoving] = useState<User | null>(null);
  const [resetting, setResetting] = useState<User | null>(null);
  // The one copy of a newly issued password. Held here rather than in the modal
  // so it survives the modal closing, and is cleared explicitly.
  const [issued, setIssued] = useState<{ email: string; password: string } | null>(null);
  const [error, setError] = useState<string | null>(null);

  const reload = () => {
    users.reload();
  };

  async function setRole(target: User, role: string) {
    setError(null);
    try {
      await api.put(`/api/users/${target.id}/role`, { role });
      reload();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not change the role.");
    }
  }

  async function resetPassword(target: User) {
    setError(null);
    setResetting(null);
    try {
      const result = await api.post<IssuedPassword>(`/api/users/${target.id}/password`);
      setIssued({ email: target.email, password: result.password });
      reload();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not reset the password.");
    }
  }

  return (
    <>
      <PageHeader
        title="Users"
        subtitle="Everyone who can sign in to this console. Access keys are managed separately."
        actions={
          <Button variant="primary" onClick={() => setCreating(true)}>
            Create a user
          </Button>
        }
      />

      {error && <ErrorNotice message={error} />}
      {users.error && <ErrorNotice message={users.error} onRetry={users.reload} />}

      <Card className="mb-[16px] overflow-hidden">
        <div className="border-b border-line-row px-[18px] py-[14px] text-[13.5px] font-semibold">
          Members
        </div>
        <TableHead columns={["Email", "Role", "Added", "Last seen", ""]} className={memberColumns} />

        {users.loading &&
          Array.from({ length: 2 }, (_, index) => (
            <div key={index} className={`grid ${memberColumns} gap-[10px] border-b border-line-row px-[18px] py-[13px]`}>
              <SkeletonLine width="55%" />
              <SkeletonLine width={50} faint />
              <SkeletonLine width={70} faint />
              <SkeletonLine width={60} faint />
              <span />
            </div>
          ))}

        {users.data?.users.map((member) => (
          <TableRow key={member.id} className={memberColumns}>
            <span className="flex min-w-0 items-center gap-[7px]">
              <span className="truncate text-[12.5px]">{member.email}</span>
              {member.id === user?.id && <Tag>you</Tag>}
            </span>
            <span>
              {member.id === user?.id ? (
                // Changing your own role is disallowed: demoting yourself as
                // the only admin would lock the console, and the server refuses
                // it anyway. Better not to offer the control at all.
                <Tag tone={member.isAdmin ? "accent" : "neutral"}>{member.role.toLowerCase()}</Tag>
              ) : (
                <Select
                  value={member.role}
                  onChange={(role) => void setRole(member, role)}
                  options={[
                    { value: "ADMIN", label: "Admin" },
                    { value: "MEMBER", label: "Member" },
                  ]}
                  ariaLabel={`Role for ${member.email}`}
                />
              )}
            </span>
            <span className="text-[12.5px] text-ink-muted">{formatDate(member.createdAt)}</span>
            <span className="text-[12.5px] text-ink-muted">{formatRelative(member.lastLoginAt)}</span>
            <span className="flex justify-end gap-[10px] text-right">
              <RowAction onClick={() => setResetting(member)}>Reset password</RowAction>
              {member.id !== user?.id && (
                <RowAction danger onClick={() => setRemoving(member)}>
                  Remove
                </RowAction>
              )}
            </span>
          </TableRow>
        ))}
      </Card>

      {creating && (
        <CreateUserModal
          onClose={() => setCreating(false)}
          onCreated={(email, password) => {
            setCreating(false);
            setIssued({ email, password });
            reload();
          }}
        />
      )}

      {resetting && (
        <Modal title={`Reset the password for ${resetting.email}`} onClose={() => setResetting(null)}>
          <div className="space-y-[16px]">
            <p className="m-0 text-[13px] leading-[1.6]">
              A new password is generated and shown once. Every device they are signed in on is
              signed out, and they must choose their own password when they next sign in.
            </p>
            <div className="flex justify-end gap-[8px]">
              <Button onClick={() => setResetting(null)}>Cancel</Button>
              <Button variant="primary" onClick={() => void resetPassword(resetting)}>
                Reset password
              </Button>
            </div>
          </div>
        </Modal>
      )}

      {issued && (
        <IssuedPasswordModal
          email={issued.email}
          password={issued.password}
          onClose={() => setIssued(null)}
        />
      )}

      {removing && (
        <Modal title={`Remove ${removing.email}`} onClose={() => setRemoving(null)}>
          <div className="space-y-[16px]">
            <p className="m-0 text-[13px] leading-[1.6]">
              They lose access immediately, and every device they are signed in on is signed out.
              Anything they created — buckets, objects, access keys — stays.
            </p>
            <div className="flex justify-end gap-[8px]">
              <Button onClick={() => setRemoving(null)}>Cancel</Button>
              <Button
                variant="danger"
                onClick={() => {
                  const target = removing;
                  setRemoving(null);
                  void api
                    .delete(`/api/users/${target.id}`)
                    .then(reload)
                    .catch((caught: unknown) =>
                      setError(caught instanceof Error ? caught.message : "Could not remove them."),
                    );
                }}
              >
                Remove member
              </Button>
            </div>
          </div>
        </Modal>
      )}
    </>
  );
}

function CreateUserModal({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (email: string, password: string) => void;
}) {
  const [email, setEmail] = useState("");
  const [role, setRole] = useState("MEMBER");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError(null);
    try {
      // No password is sent: the server generates one. An administrator
      // inventing passwords for other people tends to invent weak and reused
      // ones, and it is replaced on first sign-in anyway.
      const result = await api.post<IssuedPassword & { user: User }>("/api/users", { email, role });
      onCreated(email, result.password);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not create the user.");
      setSaving(false);
    }
  }

  return (
    <Modal title="Create a user" onClose={onClose}>
      <form className="space-y-[16px]" onSubmit={submit}>
        <Field label="Email address">
          <TextInput
            type="email"
            value={email}
            onChange={setEmail}
            placeholder="colleague@example.com"
            autoFocus
            required
          />
        </Field>
        <Field label="Role" hint="Admins can manage users and access keys. Members can use storage.">
          <Select
            value={role}
            onChange={setRole}
            options={[
              { value: "MEMBER", label: "Member" },
              { value: "ADMIN", label: "Admin" },
            ]}
            ariaLabel="Role"
          />
        </Field>
        <InfoNotice>
          A starting password is generated and shown once. There is no invitation email — pass it on
          however you already talk to them, and they must change it when they sign in.
        </InfoNotice>
        {error && <ErrorNotice message={error} />}
        <div className="flex justify-end gap-[8px]">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={saving || email.trim() === ""}>
            {saving ? "Creating…" : "Create user"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

/**
 * The one and only sight of a newly issued password.
 *
 * Only a hash is stored, so there is no second chance to read it. The modal
 * says so plainly rather than letting someone close it and find out later.
 */
function IssuedPasswordModal({
  email,
  password,
  onClose,
}: {
  email: string;
  password: string;
  onClose: () => void;
}) {
  const [copied, setCopied] = useState(false);

  return (
    <Modal title="Give them this password" onClose={onClose}>
      <div className="space-y-[16px]">
        <p className="m-0 text-[13px] leading-[1.6]">
          The password for <span className="font-medium">{email}</span>. It is shown once and is not
          stored anywhere it can be read again.
        </p>

        <div className="rounded-[12px] border border-line bg-inset px-[14px] py-[12px]">
          <code className="block break-all font-mono text-[14px] leading-[1.6]">{password}</code>
        </div>

        <InfoNotice tone="warn">
          They must change it when they first sign in, so it stops being a password you both know.
        </InfoNotice>

        <div className="flex justify-end gap-[8px]">
          <Button
            onClick={() => {
              void navigator.clipboard.writeText(password).then(() => setCopied(true));
            }}
          >
            {copied ? "Copied" : "Copy"}
          </Button>
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        </div>
      </div>
    </Modal>
  );
}
