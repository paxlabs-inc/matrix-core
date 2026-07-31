import { neutralTheme } from '@astryxdesign/theme-neutral/built'

/**
 * Keep the application root on Astryx's prebuilt theme.
 *
 * Runtime-defined themes generate and inject CSS from a client component.
 * The prebuilt artifact is both SSR-safe and already backed by the imported
 * `theme.css`; Matrix-specific typography and surface adjustments stay in
 * the app stylesheet where the local Next fonts are available.
 */
export const matrixTheme = neutralTheme
