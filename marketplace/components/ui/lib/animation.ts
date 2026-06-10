import type { Transition } from "motion/react";

/**
 * Shared motion primitives for the vendored smoothui component library.
 * Tuned to the Paxeer Brand v3 motion scale (see app/app.css --spring-*).
 * Imported by the library as `../../lib/animation`.
 */

export const DURATION_INSTANT = 0.1;
export const DURATION_SNAPPY = 0.2;
export const DURATION_STANDARD = 0.32;
export const DURATION_DELIBERATE = 0.52;

export const SPRING_DEFAULT: Transition = {
  type: "spring",
  stiffness: 200,
  damping: 30,
};

export const SPRING_SNAPPY: Transition = {
  type: "spring",
  stiffness: 350,
  damping: 35,
};

export const SPRING_BOUNCY: Transition = {
  type: "spring",
  stiffness: 400,
  damping: 28,
};

export const EASE_STANDARD = [0.32, 0.72, 0, 1] as const;
export const EASE_EMPHASIS = [0.22, 1, 0.36, 1] as const;
