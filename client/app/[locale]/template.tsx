import type { ReactNode } from 'react'

export default function LocaleTemplate({ children }: { children: ReactNode }) {
  return <div className="min-h-0 min-w-0">{children}</div>
}
