/** Human-readable byte sizes. */
export function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  // Binary units, because that is what a filesystem reports and what an
  // operator comparing against `df` will expect.
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / Math.pow(1024, exponent);
  // One decimal below 100 keeps sizes distinguishable without noise.
  const digits = value >= 100 || exponent === 0 ? 0 : 1;
  return `${value.toFixed(digits)} ${units[exponent]}`;
}

/** A date in the viewer's own locale and timezone. */
export function formatDate(iso: string | null | undefined): string {
  if (!iso) return "—";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

/** A coarse relative time, for "last used" columns. */
export function formatRelative(iso: string | null | undefined): string {
  if (!iso) return "never";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "never";

  const seconds = Math.round((Date.now() - then) / 1000);
  if (seconds < 60) return "just now";

  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ["minute", 60],
    ["hour", 3600],
    ["day", 86400],
    ["month", 2592000],
    ["year", 31536000],
  ];
  const formatter = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  let chosen: [Intl.RelativeTimeFormatUnit, number] = units[0]!;
  for (const unit of units) {
    if (seconds >= unit[1]) chosen = unit;
  }
  return formatter.format(-Math.round(seconds / chosen[1]), chosen[0]);
}

/** The trailing segment of a key, which is what reads as a filename. */
export function baseName(key: string): string {
  const trimmed = key.endsWith("/") ? key.slice(0, -1) : key;
  const index = trimmed.lastIndexOf("/");
  return index === -1 ? trimmed : trimmed.slice(index + 1);
}

/**
 * Splits a prefix into navigable breadcrumb segments.
 *
 * "photos/2026/spring/" becomes three crumbs, each carrying the full prefix
 * needed to navigate there — the object store has no folders, so a crumb is
 * just a prefix to list.
 */
export function breadcrumbs(prefix: string): { name: string; prefix: string }[] {
  if (!prefix) return [];
  const parts = prefix.split("/").filter(Boolean);
  let accumulated = "";
  return parts.map((part) => {
    accumulated += `${part}/`;
    return { name: part, prefix: accumulated };
  });
}

/** Whether a content type is worth previewing inline. */
export function previewKind(contentType: string): "image" | "text" | "pdf" | "video" | "audio" | null {
  if (contentType.startsWith("image/")) return "image";
  if (contentType.startsWith("video/")) return "video";
  if (contentType.startsWith("audio/")) return "audio";
  if (contentType === "application/pdf") return "pdf";
  if (
    contentType.startsWith("text/") ||
    contentType === "application/json" ||
    contentType === "application/xml" ||
    contentType.endsWith("+json") ||
    contentType.endsWith("+xml")
  ) {
    return "text";
  }
  return null;
}
