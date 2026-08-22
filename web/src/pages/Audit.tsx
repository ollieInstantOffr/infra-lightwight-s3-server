import { useEffect, useState } from "react";
import { api, type AuditEvent, type AuditPage } from "../lib/api";
import { formatDate } from "../lib/format";
import {
  Button,
  Card,
  EmptyState,
  ErrorNotice,
  PageHeader,
  Select,
  SkeletonLine,
  Tag,
  TableHead,
  TableRow,
} from "../components/ui";

const columns = "grid-cols-[1.4fr_1.4fr_2fr_1fr]";

export function AuditPage_() {
  const [page, setPage] = useState<AuditPage | null>(null);
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [actor, setActor] = useState("");
  const [action, setAction] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Filters reset the list rather than appending, since a filtered page has
  // nothing to do with the one before it.
  useEffect(() => {
    setLoading(true);
    setError(null);
    const query = new URLSearchParams();
    if (actor) query.set("actor", actor);
    if (action) query.set("action", action);

    api
      .get<AuditPage>(`/api/audit?${query.toString()}`)
      .then((result) => {
        setPage(result);
        setEvents(result.events);
      })
      .catch((caught: unknown) =>
        setError(caught instanceof Error ? caught.message : "Could not load the audit log."),
      )
      .finally(() => setLoading(false));
  }, [actor, action]);

  async function loadMore() {
    if (!page?.nextBefore) return;
    const query = new URLSearchParams({ before: String(page.nextBefore) });
    if (actor) query.set("actor", actor);
    if (action) query.set("action", action);

    try {
      const next = await api.get<AuditPage>(`/api/audit?${query.toString()}`);
      setEvents((current) => [...current, ...next.events]);
      setPage(next);
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Could not load more.");
    }
  }

  return (
    <>
      <PageHeader
        title="Audit log"
        subtitle="Who did what in this console. Kept for a year."
        actions={
          <>
            <Select
              value={actor}
              onChange={setActor}
              ariaLabel="Filter by person"
              options={[{ value: "", label: "Anyone" }, ...(page?.actors ?? []).map((email) => ({ value: email, label: email }))]}
            />
            <Select
              value={action}
              onChange={setAction}
              ariaLabel="Filter by action"
              options={[{ value: "", label: "Any action" }, ...(page?.actions ?? []).map((entry) => ({ value: entry.value, label: entry.label }))]}
            />
          </>
        }
      />

      {error && <ErrorNotice message={error} />}

      <Card className="overflow-hidden">
        <TableHead columns={["When", "Who", "What", "Where from"]} className={columns} />

        {loading &&
          Array.from({ length: 6 }, (_, index) => (
            <div key={index} className={`grid ${columns} gap-[10px] border-b border-line-row px-[18px] py-[13px]`}>
              <SkeletonLine width={110} />
              <SkeletonLine width="70%" faint />
              <SkeletonLine width="85%" faint />
              <SkeletonLine width={80} faint />
            </div>
          ))}

        {!loading &&
          events.map((event) => (
            <TableRow key={event.id} className={columns}>
              <span className="text-[12.5px] text-ink-muted">{formatDate(event.createdAt)}</span>
              <span className="truncate text-[12.5px]">{event.actor}</span>
              <span className="flex min-w-0 items-center gap-[7px]">
                <Tag tone={toneFor(event.action)}>{labelFor(event.action, page?.actions)}</Tag>
                {event.subject && (
                  <span className="truncate font-mono text-[11.5px] text-ink-muted" title={event.subject}>
                    {event.subject}
                  </span>
                )}
              </span>
              <span className="truncate font-mono text-[11.5px] text-ink-faint">{event.ip ?? "—"}</span>
            </TableRow>
          ))}

        {!loading && events.length === 0 && (
          <EmptyState
            title="Nothing recorded yet"
            hint="Console actions appear here as they happen. The S3 API is recorded in the server's request log instead, since it is attributable to a key rather than a person."
          />
        )}

        {page?.nextBefore && (
          <div className="border-t border-line-row px-[18px] py-[12px] text-center">
            <Button onClick={loadMore}>Load more</Button>
          </div>
        )}
      </Card>
    </>
  );
}

/** Destructive actions are tinted so a scan of the log surfaces them first. */
function toneFor(action: string): "neutral" | "accent" | "danger" {
  if (action.includes("delete") || action.includes("revoke") || action.includes("remove") || action.includes("purge")) {
    return "danger";
  }
  if (action.includes("create") || action.includes("invite") || action.includes("restore")) return "accent";
  return "neutral";
}

function labelFor(action: string, actions?: { value: string; label: string }[]): string {
  return actions?.find((entry) => entry.value === action)?.label ?? action;
}
