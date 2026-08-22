import type { ReactNode } from "react";
import { useEffect, useRef, useState } from "react";

// The primitives the Pail design is built from. Every screen composes these,
// so a change to the design system is a change here rather than in a dozen
// places.

// ─── Buttons ─────────────────────────────────────────────────────────────────

type ButtonVariant = "primary" | "secondary" | "danger" | "ghost";

const buttonStyles: Record<ButtonVariant, string> = {
  primary:
    "bg-accent text-on-accent font-semibold hover:bg-accent-hover hover:text-white",
  secondary: "border border-line-input bg-card text-ink font-medium hover:bg-inset",
  danger: "border border-danger/30 bg-danger-soft text-danger font-medium hover:bg-danger hover:text-white",
  ghost: "text-ink-muted font-medium hover:text-ink hover:bg-inset",
};

export function Button({
  children,
  onClick,
  type = "button",
  variant = "secondary",
  disabled,
  title,
  className = "",
}: {
  children: ReactNode;
  onClick?: () => void;
  type?: "button" | "submit";
  variant?: ButtonVariant;
  disabled?: boolean;
  title?: string;
  className?: string;
}) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      title={title}
      className={`inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-[10px] px-[14px] py-[9px] text-[13px] transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${buttonStyles[variant]} ${className}`}
    >
      {children}
    </button>
  );
}

/** A quieter button for table rows, where a full control would shout. */
export function RowAction({
  children,
  onClick,
  danger,
  title,
  disabled,
}: {
  children: ReactNode;
  onClick?: () => void;
  danger?: boolean;
  title?: string;
  disabled?: boolean;
}) {
  return (
    <button
      onClick={onClick}
      title={title}
      disabled={disabled}
      className={`rounded-md px-2 py-1 text-[12.5px] font-medium transition-colors disabled:opacity-50 ${
        danger ? "text-danger hover:bg-danger-soft" : "text-ink-muted hover:bg-inset hover:text-ink"
      }`}
    >
      {children}
    </button>
  );
}

// ─── Surfaces ────────────────────────────────────────────────────────────────

export function Card({
  children,
  className = "",
  padded = false,
}: {
  children: ReactNode;
  className?: string;
  padded?: boolean;
}) {
  return (
    <div
      className={`rounded-[16px] border border-line bg-card ${padded ? "p-[17px]" : ""} ${className}`}
    >
      {children}
    </div>
  );
}

export function PageHeader({
  title,
  mono,
  subtitle,
  actions,
}: {
  title: ReactNode;
  mono?: boolean;
  subtitle?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <header className="mb-[22px] flex flex-wrap items-end justify-between gap-[14px]">
      <div className="min-w-0">
        <h1
          className={`m-0 mb-[5px] truncate text-[24px] font-semibold tracking-[-0.025em] ${
            mono ? "font-mono text-[21px]" : ""
          }`}
        >
          {title}
        </h1>
        {subtitle && <p className="m-0 text-[13px] text-ink-muted">{subtitle}</p>}
      </div>
      {actions && <div className="flex flex-none flex-wrap gap-[10px]">{actions}</div>}
    </header>
  );
}

/** Breadcrumbs above a page title. */
export function Crumbs({ children }: { children: ReactNode }) {
  return (
    <nav aria-label="Breadcrumb" className="mb-[14px] flex flex-wrap items-center gap-[7px] text-[12.5px]">
      {children}
    </nav>
  );
}

export function CrumbSeparator() {
  return <span className="text-ink-faint">/</span>;
}

// ─── Tables ──────────────────────────────────────────────────────────────────

/** A column-heading row, in the design's small uppercase style. */
export function TableHead({ columns, className = "" }: { columns: ReactNode[]; className?: string }) {
  return (
    <div
      className={`grid gap-[10px] border-b border-line-row px-[18px] py-[9px] text-[10.5px] font-semibold uppercase tracking-[0.06em] text-ink-heading ${className}`}
    >
      {columns.map((column, index) => (
        <span key={index}>{column}</span>
      ))}
    </div>
  );
}

export function TableRow({
  children,
  className = "",
  onClick,
}: {
  children: ReactNode;
  className?: string;
  onClick?: () => void;
}) {
  return (
    <div
      onClick={onClick}
      className={`grid items-center gap-[10px] border-b border-line-row px-[18px] py-[13px] text-[13px] last:border-0 ${
        onClick ? "cursor-pointer hover:bg-inset" : ""
      } ${className}`}
    >
      {children}
    </div>
  );
}

// ─── Labels and status ───────────────────────────────────────────────────────

type TagTone = "neutral" | "accent" | "danger" | "warn" | "mono";

const tagTones: Record<TagTone, string> = {
  neutral: "bg-well text-ink-muted",
  accent: "bg-accent-soft text-accent-deep",
  danger: "bg-danger-soft text-danger",
  warn: "bg-warn-soft text-warn",
  mono: "bg-well text-ink-muted font-mono",
};

export function Tag({ children, tone = "neutral" }: { children: ReactNode; tone?: TagTone }) {
  return (
    <span
      className={`inline-block whitespace-nowrap rounded-[5px] px-[6px] py-[2px] text-[10px] font-semibold ${tagTones[tone]}`}
    >
      {children}
    </span>
  );
}

/** A coloured dot with a label, for health and status rows. */
export function StatusDot({ tone, children }: { tone: "ok" | "warn" | "danger"; children: ReactNode }) {
  const colour = { ok: "bg-ok", warn: "bg-warn", danger: "bg-danger" }[tone];
  return (
    <span className="inline-flex items-center gap-[7px] text-[13px]">
      <span className={`size-[7px] flex-none rounded-full ${colour}`} aria-hidden />
      {children}
    </span>
  );
}

// ─── Forms ───────────────────────────────────────────────────────────────────

export function Field({
  label,
  hint,
  children,
  error,
}: {
  label: string;
  hint?: ReactNode;
  children: ReactNode;
  error?: string | null;
}) {
  return (
    <label className="block">
      <span className="mb-[6px] block text-[12px] font-semibold text-ink-label">{label}</span>
      {children}
      {error ? (
        <span className="mt-[6px] block text-[11.5px] leading-[1.5] text-danger">{error}</span>
      ) : (
        hint && <span className="mt-[6px] block text-[11.5px] leading-[1.5] text-ink-faint">{hint}</span>
      )}
    </label>
  );
}

export function TextInput({
  value,
  onChange,
  placeholder,
  type = "text",
  autoFocus,
  required,
  name,
  mono,
  onKeyDown,
  inputRef,
  ariaLabel,
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  type?: string;
  autoFocus?: boolean;
  required?: boolean;
  name?: string;
  mono?: boolean;
  onKeyDown?: (event: React.KeyboardEvent<HTMLInputElement>) => void;
  inputRef?: React.RefObject<HTMLInputElement | null>;
  ariaLabel?: string;
}) {
  return (
    <input
      ref={inputRef}
      type={type}
      name={name}
      value={value}
      required={required}
      autoFocus={autoFocus}
      placeholder={placeholder}
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      onChange={(event) => onChange(event.target.value)}
      className={`w-full rounded-[10px] border border-line-input bg-card px-[13px] py-[11px] text-[13.5px] text-ink outline-none placeholder:text-ink-faint focus:border-accent ${
        mono ? "font-mono" : ""
      }`}
    />
  );
}

export function Select<T extends string>({
  value,
  onChange,
  options,
  ariaLabel,
}: {
  value: T;
  onChange: (value: T) => void;
  options: { value: T; label: string }[];
  ariaLabel?: string;
}) {
  return (
    <select
      value={value}
      aria-label={ariaLabel}
      onChange={(event) => onChange(event.target.value as T)}
      className="rounded-[10px] border border-line-input bg-card px-[11px] py-[9px] text-[13px] text-ink outline-none focus:border-accent"
    >
      {options.map((option) => (
        <option key={option.value} value={option.value}>
          {option.label}
        </option>
      ))}
    </select>
  );
}

export function Toggle({
  checked,
  onChange,
  label,
  description,
}: {
  checked: boolean;
  onChange: (checked: boolean) => void;
  label: string;
  description?: string;
}) {
  return (
    <label className="flex cursor-pointer items-start gap-[11px]">
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        aria-label={label}
        onClick={() => onChange(!checked)}
        className={`mt-[2px] h-[20px] w-[34px] flex-none rounded-full p-[2px] transition-colors ${
          checked ? "bg-accent" : "bg-line-input"
        }`}
      >
        <span
          className={`block size-[16px] rounded-full bg-white transition-transform ${
            checked ? "translate-x-[14px]" : ""
          }`}
        />
      </button>
      <span className="min-w-0">
        <span className="block text-[13px] font-medium">{label}</span>
        {description && <span className="mt-[2px] block text-[11.5px] leading-[1.5] text-ink-faint">{description}</span>}
      </span>
    </label>
  );
}

// ─── Feedback ────────────────────────────────────────────────────────────────

export function EmptyState({
  title,
  hint,
  action,
}: {
  title: string;
  hint?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center gap-[10px] px-6 py-[60px] text-center">
      <p className="m-0 text-[14px] font-semibold">{title}</p>
      {hint && <p className="m-0 max-w-md text-[12.5px] leading-[1.6] text-ink-muted">{hint}</p>}
      {action && <div className="mt-[6px]">{action}</div>}
    </div>
  );
}

export function ErrorNotice({
  message,
  onRetry,
  title,
}: {
  message: string;
  onRetry?: () => void;
  title?: string;
}) {
  return (
    <div
      role="alert"
      className="rounded-[12px] border border-danger/25 bg-danger-soft px-[15px] py-[13px]"
    >
      {title && <p className="m-0 mb-[3px] text-[13px] font-semibold text-danger">{title}</p>}
      <p className="m-0 text-[12.5px] leading-[1.6] text-danger">{message}</p>
      {onRetry && (
        <button onClick={onRetry} className="mt-[8px] text-[12.5px] font-semibold text-danger underline underline-offset-2">
          Try again
        </button>
      )}
    </div>
  );
}

export function InfoNotice({ children, tone = "accent" }: { children: ReactNode; tone?: "accent" | "warn" }) {
  const styles =
    tone === "warn"
      ? "border-warn/25 bg-warn-soft text-warn"
      : "border-accent/20 bg-accent-soft text-accent-deep";
  return <div className={`rounded-[12px] border px-[15px] py-[13px] text-[12.5px] leading-[1.6] ${styles}`}>{children}</div>;
}

/** The design's inline wait: a small ring plus a sentence saying what is slow. */
export function InlineSpinner({ label }: { label: string }) {
  return (
    <span className="inline-flex items-center gap-[9px]">
      <span
        aria-hidden
        className="size-[12px] flex-none rounded-full border-2 border-[#dfeae6] border-t-accent"
        style={{ animation: "pailSpin .9s linear infinite" }}
      />
      <span role="status" className="text-[11.5px] text-ink-faint">
        {label}
      </span>
    </span>
  );
}

export function Spinner({ label = "Loading" }: { label?: string }) {
  return (
    <div className="flex items-center justify-center px-6 py-[60px]">
      <InlineSpinner label={`${label}…`} />
    </div>
  );
}

/** A shimmering placeholder line, used by every skeleton screen. */
export function SkeletonLine({
  width = "100%",
  height = 11,
  faint = false,
}: {
  width?: string | number;
  height?: number;
  faint?: boolean;
}) {
  return (
    <span
      aria-hidden
      className={faint ? "skeleton-faint block" : "skeleton block"}
      style={{ width, height }}
    />
  );
}

// ─── Overlays ────────────────────────────────────────────────────────────────

export function Modal({
  title,
  subtitle,
  children,
  onClose,
  width = "max-w-[520px]",
}: {
  title: string;
  subtitle?: string;
  children: ReactNode;
  onClose: () => void;
  width?: string;
}) {
  // Escape closes, and focus moves into the dialog on open. Without both, a
  // keyboard user is stranded behind a modal they cannot dismiss.
  const panel = useRef<HTMLDivElement>(null);
  useEffect(() => {
    panel.current?.focus();
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape") onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-[#12211d]/35 p-4 backdrop-blur-[1px]"
      role="dialog"
      aria-modal="true"
      aria-label={title}
      onMouseDown={onClose}
    >
      <div
        ref={panel}
        tabIndex={-1}
        className={`w-full ${width} rounded-[16px] border border-line bg-card shadow-[0_18px_50px_rgba(18,33,29,.18)] outline-none`}
        onMouseDown={(event) => event.stopPropagation()}
      >
        <div className="border-b border-line-row px-[22px] py-[16px]">
          <h2 className="m-0 text-[19px] font-semibold tracking-[-0.02em]">{title}</h2>
          {subtitle && <p className="m-0 mt-[4px] text-[12.5px] text-ink-muted">{subtitle}</p>}
        </div>
        <div className="px-[22px] py-[18px]">{children}</div>
      </div>
    </div>
  );
}

/** A right-hand drawer, used for the object preview. */
export function Drawer({
  title,
  subtitle,
  children,
  onClose,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  children: ReactNode;
  onClose: () => void;
}) {
  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if (event.key === "Escape") onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="fixed inset-0 z-40 flex justify-end bg-[#12211d]/25" onMouseDown={onClose}>
      <aside
        className="flex h-full w-full max-w-[420px] flex-col border-l border-line bg-card shadow-[-18px_0_50px_rgba(18,33,29,.12)]"
        onMouseDown={(event) => event.stopPropagation()}
        aria-label="Object details"
      >
        <div className="flex items-start justify-between gap-3 border-b border-line-row px-[20px] py-[16px]">
          <div className="min-w-0">
            <h2 className="m-0 truncate font-mono text-[14px] font-semibold">{title}</h2>
            {subtitle && <p className="m-0 mt-[3px] text-[12px] text-ink-muted">{subtitle}</p>}
          </div>
          <button
            onClick={onClose}
            aria-label="Close"
            className="flex-none rounded-md px-2 py-1 text-[16px] leading-none text-ink-faint hover:bg-inset hover:text-ink"
          >
            ×
          </button>
        </div>
        <div className="flex-1 overflow-y-auto px-[20px] py-[18px]">{children}</div>
      </aside>
    </div>
  );
}

// ─── Copying ─────────────────────────────────────────────────────────────────

/**
 * A copy control that confirms it worked.
 *
 * Copying is invisible: without feedback a user cannot tell a successful copy
 * from a broken button, and will click it repeatedly.
 */
export function CopyButton({
  text,
  children,
  variant = "secondary",
}: {
  text: string;
  children?: ReactNode;
  variant?: ButtonVariant;
}) {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = window.setTimeout(() => setCopied(false), 1600);
    return () => window.clearTimeout(timer);
  }, [copied]);

  return (
    <Button
      variant={variant}
      onClick={() => {
        void navigator.clipboard.writeText(text).then(() => setCopied(true));
      }}
    >
      {copied ? "Copied" : (children ?? "Copy")}
    </Button>
  );
}

/** A monospace block with a copy control, for anything meant to be pasted. */
export function CodeBlock({ text, label }: { text: string; label?: string }) {
  return (
    <div>
      {label && <p className="mb-[6px] text-[11px] font-semibold uppercase tracking-[0.06em] text-ink-heading">{label}</p>}
      <div className="relative">
        <pre className="m-0 overflow-x-auto rounded-[12px] border border-line bg-inset p-[14px] font-mono text-[11.5px] leading-[1.7] text-ink">
          <code>{text}</code>
        </pre>
        <div className="absolute right-[10px] top-[10px]">
          <CopyButton text={text} />
        </div>
      </div>
    </div>
  );
}

/** A labelled monospace value with a copy control, as in the key creator. */
export function KeyValueRow({
  label,
  value,
  masked,
}: {
  label: string;
  value: string;
  masked?: boolean;
}) {
  const [revealed, setRevealed] = useState(!masked);
  const shown = revealed ? value : `${value.slice(0, 6)}${"•".repeat(Math.max(value.length - 6, 0))}`;

  return (
    <div className="flex flex-wrap items-center gap-[10px] border-b border-line-row py-[11px] last:border-0">
      <span className="w-[110px] flex-none font-mono text-[10px] font-semibold uppercase tracking-[0.06em] text-ink-heading">
        {label}
      </span>
      <span className="min-w-0 flex-1 truncate font-mono text-[12.5px]" title={revealed ? value : undefined}>
        {shown}
      </span>
      <span className="flex flex-none gap-[6px]">
        {masked && (
          <RowAction onClick={() => setRevealed((current) => !current)}>
            {revealed ? "Hide" : "Reveal"}
          </RowAction>
        )}
        <CopyButton text={value} />
      </span>
    </div>
  );
}

/**
 * A confirmation that requires typing the thing's name.
 *
 * Used only where the action destroys data. A plain "are you sure" is clicked
 * through reflexively; typing the name is a deliberate act.
 */
export function ConfirmByName({
  name,
  typed,
  onTyped,
}: {
  name: string;
  typed: string;
  onTyped: (value: string) => void;
}) {
  return (
    <Field label={`Type ${name} to confirm`}>
      <TextInput value={typed} onChange={onTyped} placeholder={name} autoFocus mono />
    </Field>
  );
}

/** A progress bar, used for disk and uploads. */
export function ProgressBar({
  fraction,
  tone = "accent",
  label,
}: {
  fraction: number;
  tone?: "accent" | "warn" | "danger";
  label?: string;
}) {
  const colour = { accent: "bg-accent", warn: "bg-warn", danger: "bg-danger" }[tone];
  return (
    <div
      className="h-[5px] overflow-hidden rounded-full bg-[#e2ebe8]"
      role="progressbar"
      aria-valuenow={Math.round(fraction * 100)}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-label={label}
    >
      <div className={`h-full ${colour} transition-[width]`} style={{ width: `${Math.min(Math.max(fraction, 0), 1) * 100}%` }} />
    </div>
  );
}

/** Tabs, as used on the bucket screen. */
export function Tabs<T extends string>({
  tabs,
  active,
  onChange,
}: {
  tabs: { id: T; label: string }[];
  active: T;
  onChange: (id: T) => void;
}) {
  return (
    <div className="mb-[18px] flex gap-[4px] border-b border-line">
      {tabs.map((tab) => (
        <button
          key={tab.id}
          onClick={() => onChange(tab.id)}
          className={`-mb-px border-b-2 px-[13px] py-[9px] text-[13px] transition-colors ${
            active === tab.id
              ? "border-accent font-semibold text-ink"
              : "border-transparent font-medium text-ink-muted hover:text-ink"
          }`}
        >
          {tab.label}
        </button>
      ))}
    </div>
  );
}
