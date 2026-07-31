import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import path from 'path'

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./tests/setup.ts'],
    include: ['tests/**/*.{test,spec}.{ts,tsx}', 'components/**/*.{test,spec}.{ts,tsx}'],
    coverage: {
      provider: 'v8',
      reporter: ['text', 'lcov'],
      include: ['lib/**', 'components/matrix/**', 'hooks/**'],
      thresholds: {
        lines: Number(process.env.COVERAGE_MIN ?? 20),
        functions: Number(process.env.COVERAGE_MIN ?? 12),
        statements: Number(process.env.COVERAGE_MIN ?? 20),
        branches: Number(process.env.COVERAGE_MIN ?? 14),
      },
    },
  },
  resolve: {
    alias: {
      '@': path.resolve(__dirname, '.'),
      // next-intl's dev-mode ESM build imports the bare specifier
      // 'next/navigation' (no extension). Next's package.json has no
      // "exports" map, so Vite's strict ESM resolver can't find it from
      // inside next-intl's nested node_modules — redirect to the concrete
      // file, which does exist.
      'next/navigation': path.resolve(__dirname, 'node_modules/next/navigation.js'),
    },
  },
  ssr: {
    noExternal: ['next-intl'],
  },
})
