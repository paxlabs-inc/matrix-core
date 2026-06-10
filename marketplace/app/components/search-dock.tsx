import { useRef } from "react";
import { useNavigate } from "react-router";
import MorphSurface from "../../components/ui/smoothui/ai-input";

/**
 * Plain-language search built on the smoothui `ai-input` morph dock.
 * The vendored component owns its form and never surfaces the typed value,
 * so this wrapper listens in the capture phase (submit + plain Enter) and
 * routes the query to /discoverno edits to the vendored component.
 */
export function SearchDock({ className }: { className?: string }) {
  const wrapRef = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();

  function search() {
    const value = wrapRef.current?.querySelector("textarea")?.value.trim();
    if (value) navigate(`/discover?q=${encodeURIComponent(value)}`);
  }

  return (
    // biome-ignore-style: capture-phase listeners bridge the vendored form.
    <div
      ref={wrapRef}
      className={className}
      onSubmitCapture={search}
      onKeyDownCapture={(e) => {
        if (
          e.key === "Enter" &&
          !e.shiftKey &&
          !e.metaKey &&
          e.target instanceof HTMLTextAreaElement
        ) {
          e.preventDefault();
          e.stopPropagation();
          search();
        }
      }}
    >
      <MorphSurface />
    </div>
  );
}
