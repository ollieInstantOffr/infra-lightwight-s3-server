import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, type SearchResults } from "../lib/api";
import { formatBytes } from "../lib/format";
import { InlineSpinner } from "./ui";

// Search across every bucket. The palette is the only way to find an object
// whose bucket you have forgotten, so it has to be reachable from anywhere and
// honest about what it did not look at.

export function CommandPalette({ onClose }: { onClose: () => void }) {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<SearchResults | null>(null);
  const [searching, setSearching] = useState(false);
  const [highlighted, setHighlighted] = useState(0);
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    input.current?.focus();
  }, []);

  // Debounced: a search on every keystroke would scan the object table once
  // per character.
  useEffect(() => {
    if (query.trim() === "") {
      setResults(null);
      return;
    }
    const controller = new AbortController();
    const timer = window.setTimeout(() => {
      setSearching(true);
      api
        .get<SearchResults>(`/api/search?q=${encodeURIComponent(query)}`, controller.signal)
        .then((found) => {
          setResults(found);
          setHighlighted(0);
        })
        .catch(() => setResults(null))
        .finally(() => setSearching(false));
    }, 180);

    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [query]);

  const hits = useMemo(() => results?.hits ?? [], [results]);

  function open(index: number) {
    const hit = hits[index];
    if (!hit) return;
    const prefix = hit.key.includes("/") ? hit.key.slice(0, hit.key.lastIndexOf("/") + 1) : "";
    navigate(`/buckets/${encodeURIComponent(hit.bucket)}/${prefix}`);
    onClose();
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center bg-[#12211d]/35 p-4 pt-[12vh] backdrop-blur-[1px]"
      role="dialog"
      aria-modal="true"
      aria-label="Search objects"
      onMouseDown={onClose}
    >
      <div
        className="w-full max-w-[620px] overflow-hidden rounded-[16px] border border-line bg-card shadow-[0_18px_50px_rgba(18,33,29,.2)]"
        onMouseDown={(event) => event.stopPropagation()}
      >
        <div className="flex items-center gap-[10px] border-b border-line-row px-[16px] py-[13px]">
          <span className="text-[14px] text-ink-faint" aria-hidden>
            ⌕
          </span>
          <input
            ref={input}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Search object keys across every bucket"
            aria-label="Search object keys"
            className="w-full bg-transparent font-mono text-[13.5px] outline-none placeholder:font-sans placeholder:text-ink-faint"
            onKeyDown={(event) => {
              if (event.key === "Escape") onClose();
              if (event.key === "ArrowDown") {
                event.preventDefault();
                setHighlighted((current) => Math.min(current + 1, hits.length - 1));
              }
              if (event.key === "ArrowUp") {
                event.preventDefault();
                setHighlighted((current) => Math.max(current - 1, 0));
              }
              if (event.key === "Enter") {
                event.preventDefault();
                open(highlighted);
              }
            }}
          />
          <kbd className="rounded-[5px] border border-line bg-well px-[6px] py-[2px] font-mono text-[10px] text-ink-faint">
            esc
          </kbd>
        </div>

        <div className="max-h-[52vh] overflow-y-auto">
          {searching && (
            <div className="px-[16px] py-[18px]">
              <InlineSpinner label="Searching…" />
            </div>
          )}

          {!searching && query.trim() !== "" && hits.length === 0 && (
            <p className="px-[16px] py-[22px] text-center text-[12.5px] text-ink-muted">
              Nothing matches “{query}”.
            </p>
          )}

          {hits.map((hit, index) => (
            <button
              key={`${hit.bucket}/${hit.key}`}
              onMouseEnter={() => setHighlighted(index)}
              onClick={() => open(index)}
              className={`flex w-full items-center gap-[12px] px-[16px] py-[10px] text-left ${
                index === highlighted ? "bg-accent-soft" : "hover:bg-inset"
              }`}
            >
              <span className="w-[110px] flex-none truncate font-mono text-[11px] text-ink-muted">
                {hit.bucket}
              </span>
              <span className="min-w-0 flex-1 truncate font-mono text-[12.5px]">{hit.key}</span>
              <span className="flex-none text-[11.5px] tabular-nums text-ink-faint">
                {formatBytes(hit.size)}
              </span>
            </button>
          ))}

          {/* Saying so matters: a silently capped search reads as "not here". */}
          {results?.truncated && (
            <p className="border-t border-line-row px-[16px] py-[10px] text-[11.5px] text-ink-faint">
              Showing the first {hits.length} matches. Narrow the search to see more.
            </p>
          )}
        </div>
      </div>
    </div>
  );
}
