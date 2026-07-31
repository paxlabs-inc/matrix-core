'use client'

/**
 * Sign-in surface — the auth controls (OAuth + magic-link) that sit in the
 * left column of the split login page. Layers separate by background tone;
 * no gradients, no glow, no strokes.
 */
import { useState } from 'react'
import { useSearchParams } from 'next/navigation'
import { useLocale } from 'next-intl'
import { Card, VStack } from '@astryxdesign/core/Layout'
import { FormLayout } from '@astryxdesign/core/FormLayout'
import { TextInput } from '@astryxdesign/core/TextInput'
import { Button } from '@astryxdesign/core/Button'
import { Text } from '@astryxdesign/core/Text'
import { useRouter } from '@/i18n/navigation'
import { Github } from '@/lib/matrix-icons'
import { signInWithEmail, signInWithOAuth } from '@/lib/auth/session'
import { authEnabled } from '@/lib/env'
import { stripLocalePrefixFromHref } from '@/lib/i18n/proxy-path'

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
    <Card variant="muted" padding={5} elevation="none" className="w-full">
      <VStack gap={4} width="100%">
        {sent ? (
          <Text type="body" color="secondary">
            Check <Text weight="medium">{email}</Text> for the magic link.
          </Text>
        ) : (
          <>
            <VStack gap={2} width="100%">
              <Button
                label={oauth === 'google' ? 'Redirecting…' : 'Continue with Google'}
                type="button"
                variant="secondary"
                width="100%"
                isDisabled={!!oauth || submitting}
                isLoading={oauth === 'google'}
                icon={<GoogleGlyph />}
                onClick={() => oauthSignIn('google')}
              />
              <Button
                label={oauth === 'github' ? 'Redirecting…' : 'Continue with GitHub'}
                type="button"
                variant="secondary"
                width="100%"
                isDisabled={!!oauth || submitting}
                isLoading={oauth === 'github'}
                icon={<Github className="size-4" />}
                onClick={() => oauthSignIn('github')}
              />
            </VStack>

            <Text type="supporting" color="secondary" display="block" justify="center">
              or
            </Text>

            <form onSubmit={submit}>
              <FormLayout>
                <TextInput
                  id="email"
                  type="email"
                  label="Email"
                  description="We don’t store passwords. The router verifies every request."
                  htmlName="email"
                  value={email}
                  onChange={setEmail}
                  placeholder="you@example.com"
                  isRequired
                  status={error ? { type: 'error', message: error } : undefined}
                  width="100%"
                />
                <Button
                  label={submitting ? 'Sending…' : 'Send magic link'}
                  type="submit"
                  isDisabled={submitting || !!oauth}
                  isLoading={submitting}
                  width="100%"
                />
              </FormLayout>
            </form>
          </>
        )}
        {process.env.NODE_ENV !== 'production' && !sent && (
          <Button
            label="Continue without auth (dev only)"
            type="button"
            onClick={continueAsDev}
            variant="ghost"
            size="sm"
            width="100%"
          />
        )}
      </VStack>
    </Card>
  )
}
