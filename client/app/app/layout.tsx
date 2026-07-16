/**
 * Root layout — pass-through so `app/[locale]/layout.tsx` owns <html>/<body>
 * and the active `lang` attribute (next-intl URL segment routing).
 */
export default function RootLayout({ children }: { children: React.ReactNode }) {
  return children
}
