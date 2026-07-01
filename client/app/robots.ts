import type { MetadataRoute } from 'next'

export default function robots(): MetadataRoute.Robots {
  return {
    rules: {
      // Block all crawlers until the app is publicly launched.
      // Flip to allow: ['/'] and add a sitemap before going public.
      userAgent: '*',
      disallow: '/',
    },
  }
}
