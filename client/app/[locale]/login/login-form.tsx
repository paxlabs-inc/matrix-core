'use client'

/**
 * Sign-in surface — single-column magic-link form. Visually consistent
 * with the rest of the dashboard's dark, high-signal aesthetic; no
 * gradients, no glow.
 */
import { useState } from 'react'
import { useSearchParams } from 'next/navigation'
import { useLocale } from 'next-intl'
import { useRouter } from '@/i18n/navigation'
import { Github } from '@/lib/matrix-icons'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from '@/components/ui/card'
import { Field, FieldGroup, FieldLabel, FieldDescription } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'
import { signInWithEmail, signInWithOAuth } from '@/lib/auth/session'
import { authEnabled } from '@/lib/env'
import { stripLocalePrefixFromHref } from '@/lib/i18n/proxy-path'
import { MatrixLogo } from '@/components/matrix/matrix-logo'

type OAuthProvider = 'google' | 'github'

/** Same-origin callback URL the provider returns to. Locale-prefixed so
 *  the implicit-flow hash lands on a public segment (see proxy.ts). */
function callbackUrl(locale: string, next: string): string | undefined {
  if (typeof window === 'undefined') return undefined
  return `${window.location.origin}/${locale}/auth/callback?next=${encodeURIComponent(next)}`
}

function GoogleGlyph() {
  return (
    <svg viewBox="0 0 24 24" className="size-4" aria-hidden>
      <path
        fill="#4285F4"
        d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92a5.06 5.06 0 0 1-2.2 3.32v2.77h3.57c2.08-1.92 3.27-4.74 3.27-8.1Z"
      />
      <path
        fill="#34A853"
        d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84A11 11 0 0 0 12 23Z"
      />
      <path
        fill="#FBBC05"
        d="M5.84 14.1a6.6 6.6 0 0 1 0-4.2V7.06H2.18a11 11 0 0 0 0 9.88l3.66-2.84Z"
      />
      <path
        fill="#EA4335"
        d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1A11 11 0 0 0 2.18 7.06l3.66 2.84C6.71 7.3 9.14 5.38 12 5.38Z"
      />
    </svg>
  )
}

export default function LoginForm() {
  const router = useRouter()
  const locale = useLocale()
  const params = useSearchParams()
  // Defensively strip any locale prefix from `next`: navigation below goes
  // through the i18n router, which re-prefixes the active locale — a
  // prefixed value would produce /en/en/... and 404. (Older deployed links
  // and bookmarks may still carry the prefixed form.)
  const next = stripLocalePrefixFromHref(params.get('next') ?? '/')

  const [email, setEmail] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [sent, setSent] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [oauth, setOauth] = useState<OAuthProvider | null>(null)

  async function oauthSignIn(provider: OAuthProvider) {
    if (oauth || submitting) return
    setError(null)
    if (!authEnabled) {
      setError('Auth is not configured. Set NEXT_PUBLIC_SUPABASE_URL + ANON_KEY to sign in.')
      return
    }
    setOauth(provider)
    try {
      // Redirects the browser to the provider; control does not return
      // here on success — the provider sends the user to /auth/callback.
      await signInWithOAuth(provider, callbackUrl(locale, next))
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setOauth(null)
    }
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (submitting) return
    setError(null)
    if (!authEnabled) {
      setError('Auth is not configured. Set NEXT_PUBLIC_SUPABASE_URL + ANON_KEY to sign in.')
      return
    }
    if (!/.+@.+\..+/.test(email)) {
      setError('Enter a valid email address.')
      return
    }
    setSubmitting(true)
    try {
      await signInWithEmail(email.trim(), callbackUrl(locale, next))
      setSent(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setSubmitting(false)
    }
  }

  function continueAsDev() {
    // Local-only escape hatch: sets the dev marker and bounces forward.
    document.cookie = 'mx-session=dev; Path=/; Max-Age=86400; SameSite=Lax'
    router.replace(next)
  }

  return (
    <Card className="w-full max-w-sm">
      <CardHeader className="items-center text-center">
        <div className="mb-2 flex justify-center">
          <MatrixLogo size="lg" />
        </div>
        <CardTitle className="text-base">Sign in to Matrix</CardTitle>
        <CardDescription>We&rsquo;ll email you a one-time link.</CardDescription>
      </CardHeader>
      <CardContent>
        {sent ? (
          <p className="text-muted-foreground text-sm">
            Check <span className="text-foreground font-medium">{email}</span> for the magic link.
          </p>
        ) : (
          <>
            <div className="flex flex-col gap-2">
              <Button
                type="button"
                variant="outline"
                className="w-full"
                disabled={!!oauth || submitting}
                onClick={() => oauthSignIn('google')}
              >
                <GoogleGlyph />
                {oauth === 'google' ? 'Redirecting\u2026' : 'Continue with Google'}
              </Button>
              <Button
                type="button"
                variant="outline"
                className="w-full"
                disabled={!!oauth || submitting}
                onClick={() => oauthSignIn('github')}
              >
                <Github className="size-4" />
                {oauth === 'github' ? 'Redirecting\u2026' : 'Continue with GitHub'}
              </Button>
            </div>

            <div className="my-4 flex items-center gap-3">
              <span className="bg-border h-px flex-1" />
              <span className="text-muted-foreground text-xs">or</span>
              <span className="bg-border h-px flex-1" />
            </div>

            <form onSubmit={submit}>
              <FieldGroup>
                <Field>
                  <FieldLabel htmlFor="email">Email</FieldLabel>
                  <Input
                    id="email"
                    type="email"
                    inputMode="email"
                    autoComplete="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    placeholder="you@example.com"
                    required
                  />
                  <FieldDescription>
                    We don&rsquo;t store passwords. The router verifies every request.
                  </FieldDescription>
                </Field>
                {error && (
                  <p className="text-destructive text-sm" role="alert">
                    {error}
                  </p>
                )}
                <Button type="submit" disabled={submitting || !!oauth} className="w-full">
                  {submitting ? 'Sending\u2026' : 'Send magic link'}
                </Button>
              </FieldGroup>
            </form>
          </>
        )}
        {process.env.NODE_ENV !== 'production' && !sent && (
          <button
            type="button"
            onClick={continueAsDev}
            className="text-muted-foreground hover:text-foreground mt-4 w-full text-center text-xs underline-offset-4 hover:underline"
          >
            Continue without auth (dev only)
          </button>
        )}
      </CardContent>
    </Card>
  )
}
