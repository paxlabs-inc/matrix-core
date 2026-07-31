import type { MetadataRoute } from 'next'

export default function manifest(): MetadataRoute.Manifest {
  return {
    name: 'Matrix — agent operations',
    short_name: 'Matrix',
    description:
      'Dispatch agents to complete real tasks, watch them run to completion, and collect signed, replay-tested receipts.',
    start_url: '/en',
    display: 'standalone',
    orientation: 'portrait-primary',
    background_color: '#29303e',
    theme_color: '#0a0a0b',
    categories: ['productivity', 'utilities'],
    icons: [
      {
        src: '/icon-light-32x32.png',
        sizes: '32x32',
        type: 'image/png',
        purpose: 'any',
      },
      {
        src: '/apple-icon.png',
        sizes: '180x180',
        type: 'image/png',
        purpose: 'any',
      },
      {
        src: '/icon.svg',
        sizes: 'any',
        type: 'image/svg+xml',
        purpose: 'any',
      },
    ],
  }
}
