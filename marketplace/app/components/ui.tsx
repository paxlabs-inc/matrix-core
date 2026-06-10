import { cn } from "@/lib/utils";
import { statusTone } from "@/lib/format";
import type { ReactNode } from "react";
import ButtonCopy from "../../components/ui/smoothui/button-copy";

/**
 * Shared presentational primitives for both surfaces. Anything with a
 * smoothui equivalent delegates to the vendored library; what remains here is
 * layout/typography glue. Depth comes from the v3 surface-tone ladder
 * (background → card → secondary) plus soft shadowsnever border strokes
 * (Paxeer Brand v3 hard rule).
 */

export function Surface({
  className,
  as: As = "div",
  ...props
}: { className?: string; as?: "div" | "section" | "article" } & React.HTMLAttributes<HTMLDivElement>) {
  return (
    <As
      className={cn("rounded-xl bg-card text-card-foreground shadow-02", className)}
      {...props}
    />
  );
}

export function Eyebrow({ children, className }: { children: ReactNode; className?: string }) {
  return <p className={cn("eyebrow", className)}>{children}</p>;
}

export function SectionHeading({
  eyebrow,
  title,
  description,
  className,
  align = "left",
}: {
  eyebrow?: string;
  title: ReactNode;
  description?: ReactNode;
  className?: string;
  align?: "left" | "center";
}) {
  return (
    <div className={cn("flex flex-col gap-3", align === "center" && "items-center text-center", className)}>
      {eyebrow ? <Eyebrow>{eyebrow}</Eyebrow> : null}
      <h2 className="text-h2 text-foreground">{title}</h2>
      {description ? (
        <p className={cn("body text-muted-foreground", align === "center" && "max-w-2xl")}>{description}</p>
      ) : null}
    </div>
  );
}

type BadgeTone = "neutral" | "accent" | "success" | "warning" | "danger" | "muted";

const badgeTones: Record<BadgeTone, string> = {
  neutral: "bg-secondary text-foreground",
  muted: "bg-secondary text-muted-foreground",
  accent: "bg-[color:var(--accent-tint)] text-[color:var(--accent-fore)]",
  success: "bg-[color:color-mix(in_oklab,var(--success)_18%,transparent)] text-[color:var(--success)]",
  warning: "bg-[color:color-mix(in_oklab,var(--warning)_20%,transparent)] text-[color:var(--warning)]",
  danger: "bg-[color:color-mix(in_oklab,var(--danger)_18%,transparent)] text-[color:var(--danger)]",
};

export function Badge({
  children,
  tone = "neutral",
  className,
}: {
  children: ReactNode;
  tone?: BadgeTone;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-2 rounded-full px-3 py-1 text-xs font-medium",
        badgeTones[tone],
        className
      )}
    >
      {children}
    </span>
  );
}

export function StatusBadge({ status }: { status: string }) {
  const tone = statusTone(status);
  const map: Record<string, BadgeTone> = {
    active: "success",
    draft: "warning",
    paused: "warning",
    delisted: "danger",
    neutral: "muted",
  };
  return (
    <Badge tone={map[tone]}>
      <span className="size-1.5 rounded-full bg-current" />
      {status.charAt(0).toUpperCase() + status.slice(1)}
    </Badge>
  );
}

export function KindBadge({ kind }: { kind: string }) {
  return (
    <Badge tone={kind === "agent" ? "accent" : "neutral"}>
      {kind === "agent" ? "Agent" : "Data"}
    </Badge>
  );
}

export function Stat({
  label,
  value,
  hint,
  className,
}: {
  label: string;
  value: ReactNode;
  hint?: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex flex-col gap-1", className)}>
      <span className="eyebrow">{label}</span>
      <span className="text-h3 text-foreground tabular-nums">{value}</span>
      {hint ? <span className="body-sm text-muted-foreground">{hint}</span> : null}
    </div>
  );
}

export function EmptyState({
  icon,
  title,
  description,
  action,
  className,
}: {
  icon?: ReactNode;
  title: string;
  description?: string;
  action?: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center gap-4 rounded-xl bg-card px-6 py-16 text-center shadow-01",
        className
      )}
    >
      {icon ? (
        <div className="flex size-12 items-center justify-center rounded-full bg-secondary text-muted-foreground">
          {icon}
        </div>
      ) : null}
      <div className="flex flex-col gap-2">
        <h3 className="text-h3 text-foreground">{title}</h3>
        {description ? <p className="body-sm max-w-md text-muted-foreground">{description}</p> : null}
      </div>
      {action}
    </div>
  );
}

export function Spinner({ className }: { className?: string }) {
  return (
    <span
      className={cn(
        "inline-block size-4 animate-spin rounded-full border-2 border-current border-t-transparent",
        className
      )}
      aria-hidden
    />
  );
}

/** Copy-to-clipboard chipsmoothui `button-copy` bound to a value. */
export function CopyChip({ value, className }: { value: string; className?: string }) {
  return (
    <ButtonCopy
      className={className}
      onCopy={() => navigator.clipboard?.writeText(value)}
      loadingDuration={150}
    />
  );
}
