import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/**
 * Merge class names with Tailwind-aware conflict resolution.
 * Aliased as `@repo/shadcn-ui/lib/utils` for the vendored smoothui library.
 */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
