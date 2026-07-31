/** @type {import('@lhci/cli').CiConfig} */
const { chromium } = require('@playwright/test')

module.exports = {
  ci: {
    collect: {
      chromePath: chromium.executablePath(),
      url: ['http://127.0.0.1:3000/en/workforce?dev=1'],
      startServerCommand: 'node scripts/lighthouse-server.mjs',
      startServerReadyPattern: 'Lighthouse proxy ready',
      startServerReadyTimeout: 120_000,
      numberOfRuns: 3,
      settings: {
        chromeFlags: '--no-sandbox --headless',
      },
    },
    assert: {
      assertions: {
        'categories:performance': ['error', { minScore: 0.65 }],
        'categories:accessibility': ['error', { minScore: 0.9 }],
        'first-contentful-paint': ['error', { maxNumericValue: 2500 }],
        'largest-contentful-paint': ['error', { maxNumericValue: 2500 }],
        'cumulative-layout-shift': ['error', { maxNumericValue: 0.1 }],
        'total-blocking-time': ['error', { maxNumericValue: 300 }],
        'speed-index': ['error', { maxNumericValue: 3500 }],
      },
    },
    upload: {
      target: 'filesystem',
      outputDir: './lhci-reports',
    },
  },
}
