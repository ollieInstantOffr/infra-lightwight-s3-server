import { useState } from "react";
import { api, type Invite, type User } from "../lib/api";
import { useApi } from "../lib/useApi";
import { useSession } from "../lib/session";
import { formatDate, formatRelative } from "../lib/format";
import {
  Button,
  Card,
  EmptyState,
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
const inviteColumns = "grid-cols-[2fr_.8fr_1fr_auto]";

export function UsersPage() {
  const { user } = useSession();
  const users = useApi<{ users: User[] }>("/api/users");
  const invites = useApi<{ invites: Invite[] }>("/api/invites");
  const [inviting, setInviting] = useState(false);
  const [removing, setRemoving] = useState<User | null>(null);
  const [error, setError] = useState<string | null>(null);

  const reload = () => {
    users.reload();
    invites.reload();
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

  async function withdraw(invite: Invite) {
    setError(null);
    try {
      await api.delete(`/api/invites/${invite.id}`);
      invites.reload();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not withdraw the invitation.");
    }
  }

  return (
    <>
      <PageHeader
        title="Users"
        subtitle="Everyone who can sign in to this console. Access keys are managed separately."
        actions={
          <Button variant="primary" onClick={() => setInviting(true)}>
            Invite someone
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
            <span className="text-right">
              {member.id !== user?.id && (
                <RowAction danger onClick={() => setRemoving(member)}>
                  Remove
                </RowAction>
              )}
            </span>
          </TableRow>
        ))}
      </Card>

      <Card className="overflow-hidden">
        <div className="border-b border-line-row px-[18px] py-[14px] text-[13.5px] font-semibold">
          Pending invitations
        </div>
        {invites.data && invites.data.invites.length > 0 ? (
          <>
            <TableHead columns={["Email", "Role", "Expires", ""]} className={inviteColumns} />
            {invites.data.invites.map((invite) => (
              <TableRow key={invite.id} className={inviteColumns}>
                <span className="truncate text-[12.5px]">{invite.email}</span>
                <span>
                  <Tag>{invite.role.toLowerCase()}</Tag>
                </span>
                <span className="text-[12.5px] text-ink-muted">{formatRelative(invite.expiresAt)}</span>
                <span className="text-right">
                  <RowAction danger onClick={() => void withdraw(invite)}>
                    Withdraw
                  </RowAction>
                </span>
              </TableRow>
            ))}
          </>
        ) : (
          <EmptyState title="No pending invitations" />
        )}
      </Card>

      {inviting && (
        <InviteModal
          onClose={() => setInviting(false)}
          onInvited={() => {
            setInviting(false);
            reload();
          }}
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

function InviteModal({ onClose, onInvited }: { onClose: () => void; onInvited: () => void }) {
  const [email, setEmail] = useState("");
  const [role, setRole] = useState("MEMBER");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError(null);
    try {
      await api.post("/api/users/invite", { email, role });
      onInvited();
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not send the invitation.");
      setSaving(false);
    }
  }

  return (
    <Modal title="Invite someone" onClose={onClose}>
      <form className="space-y-[16px]" onSubmit={submit}>
        <Field label="Email address">
          <TextInput type="email" value={email} onChange={setEmail} placeholder="colleague@example.com" autoFocus required />
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
          They receive a link that both accepts the invitation and signs them in, so there is no
          separate sign-up step. It expires in 7 days.
        </InfoNotice>
        {error && <ErrorNotice message={error} />}
        <div className="flex justify-end gap-[8px]">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={saving || email.trim() === ""}>
            {saving ? "Sending…" : "Send invitation"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
