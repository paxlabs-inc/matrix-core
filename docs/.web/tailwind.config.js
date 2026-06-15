/** @type {import('tailwindcss').Config} */

export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        bg: {
          base: '#0d0d0d',
          surface: '#141414',
          'surface-hover': '#1a1a1a',
          'surface-active': '#1f1f1f',
          code: '#0a0a0a',
        },
        border: {
          subtle: '#1f1f1f',
          DEFAULT: '#2a2a2a',
          bright: '#3a3a3a',
        },
        fg: {
          primary: '#ededed',
          secondary: '#a0a0a0',
          tertiary: '#6e6e6e',
          muted: '#4a4a4a',
        },
        accent: {
          DEFAULT: '#e06230',
          hover: '#e87840',
          subtle: 'rgba(224, 98, 48, 0.1)',
        },
        success: '#22c55e',
        warning: '#f59e0b',
        error: '#ef4444',
      },
      fontFamily: {
        wave: ['LTWave-Regular', 'LTWave-Medium'],
        sans: ['CursorGothic', 'Inter', 'system-ui', '-apple-system', 'sans-serif'],
        mono: ['JetBrains Mono', 'SF Mono', 'Menlo', 'monospace'],
      },
      fontSize: {
        'docs-xs': ['13px', { lineHeight: '1.5', letterSpacing: '0' }],
        'docs-sm': ['14px', { lineHeight: '1.6', letterSpacing: '0' }],
        'docs-base': ['15px', { lineHeight: '1.7', letterSpacing: '0' }],
        'docs-lg': ['16px', { lineHeight: '1.6', letterSpacing: '-0.01em' }],
        'docs-xl': ['18px', { lineHeight: '1.4', letterSpacing: '-0.02em' }],
        'docs-2xl': ['24px', { lineHeight: '1.2', letterSpacing: '-0.02em' }],
        'docs-3xl': ['32px', { lineHeight: '1.15', letterSpacing: '-0.03em' }],
      },
      maxWidth: {
        docs: '760px',
        sidebar: '260px',
      },
      borderRadius: {
        docs: '10px',
        'docs-sm': '6px',
      },
    },
  },
  plugins: [],
};
