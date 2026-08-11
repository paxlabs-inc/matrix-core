import type { MetadataRoute } from 'next'
import { BRAND_DESCRIPTION, BRAND_NAME, BRAND_SHORT_NAME } from '@/lib/brand'

export default function manifest(): MetadataRoute.Manifest {
  return {
    name: `${BRAND_NAME} — agent operations`,
    short_name: BRAND_SHORT_NAME,
    description: BRAND_DESCRIPTION,
    start_url: '/en',
    display: 'standalone',
    orientation: 'portrait-primary',
    background_color: '#29303e',
    theme_color: '#0a0a0b',
    categories: ['productivity', 'utilities'],
    icons: [{ src: '/centra-icon.svg', sizes: 'any', type: 'image/svg+xml', purpose: 'any' }],
  }
}
