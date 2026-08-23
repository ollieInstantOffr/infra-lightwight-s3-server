/**
 * The Pail mark: three stacked bars, the third one short.
 *
 * Design 6b, "objects in one volume" — the short third bar is the idea, so it
 * is the part that must survive shrinking. It is drawn inline rather than
 * loaded from a file because the console runs air-gapped and a mark that
 * arrives one request later than the layout flashes on every page load.
 */
export function Logo({ size = 26, className }: { size?: number; className?: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 64 64"
      fill="none"
      className={className}
      // The wordmark sits next to this everywhere it is used, so the mark adds
      // nothing for a screen reader beyond repeating it.
      aria-hidden="true"
      focusable="false"
    >
      <rect width="64" height="64" rx="16" fill="var(--color-ink)" />
      <rect x="14" y="17" width="36" height="9" rx="4.5" fill="var(--color-accent-light)" />
      <rect x="14" y="29.5" width="36" height="9" rx="4.5" fill="var(--color-accent)" />
      <rect x="14" y="42" width="22" height="9" rx="4.5" fill="var(--color-accent)" opacity=".45" />
    </svg>
  );
}
