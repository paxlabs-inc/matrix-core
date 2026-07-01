import { Suspense } from 'react'
import LoginForm from './login-form'

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

export const metadata = {
  title: 'Sign in',
}

export default function LoginPage() {
  return (
    <main className="bg-background flex min-h-screen items-center justify-center p-6">
      <Suspense fallback={null}>
        <LoginForm />
      </Suspense>
    </main>
  )
}
