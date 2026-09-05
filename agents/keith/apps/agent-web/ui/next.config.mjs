/** @type {import('next').NextConfig} */
const nextConfig = {
  output: 'export',
  assetPrefix: '/assets/ui',
  images: { unoptimized: true },
  poweredByHeader: false,
  reactStrictMode: true,
}

export default nextConfig
