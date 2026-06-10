/**
 * Local shim for `@smoothui/data` (the upstream demo-data package, which is
 * not published for this monorepo layout). Only the handful of demo
 * components in the vendored library reference it; the Deus marketplace
 * screens supply their own data. Permissive types keep those demo
 * components type-checking without pulling an external dependency.
 */

export interface Person {
  [key: string]: unknown;
  id?: string;
  name?: string;
  role?: string;
  title?: string;
  company?: string;
  handle?: string;
  username?: string;
  avatar?: string;
  image?: string;
  bio?: string;
}

export type Testimonial = Record<string, unknown>;

/** Resolve an ImageKit-style asset path to a usable URL. */
export function getImageKitUrl(path: string, _opts?: Record<string, unknown>): string {
  if (!path) return "";
  if (path.startsWith("http://") || path.startsWith("https://")) return path;
  return path.startsWith("/") ? path : `/${path}`;
}

/** Deterministic placeholder avatar URL for a seed. */
export function getAvatarUrl(seed: string): string {
  const s = encodeURIComponent(seed || "anon");
  return `https://api.dicebear.com/9.x/glass/svg?seed=${s}`;
}

export function getTestimonials(): Testimonial[] {
  return [];
}

export function getAllPeople(): Person[] {
  return [];
}
