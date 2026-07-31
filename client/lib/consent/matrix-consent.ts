export const CONSENT_COOKIE = 'mx_consent'
export const CONSENT_VERSION = 1
export const CONSENT_MAX_AGE = 60 * 60 * 24 * 365

export type ConsentCategory = 'functional' | 'analytics'

export type MatrixConsentState = {
  v: number
  necessary: true
  functional: boolean
  analytics: boolean
  ts: number
}

export type MatrixConsentChoice = Pick<MatrixConsentState, 'functional' | 'analytics'>

type ConsentListener = (consent: MatrixConsentState) => void

export type MatrixConsentApi = {
  get: () => MatrixConsentState | null
  has: () => boolean
  allowed: (cat: ConsentCategory) => boolean
  openPreferences: () => void
  onChange: (fn: ConsentListener) => () => void
  reset: () => void
}

declare global {
  interface Window {
    MatrixConsent?: MatrixConsentApi
  }
}

const listeners = new Set<ConsentListener>()
let openPreferencesHandler: (() => void) | null = null

function readCookie(): MatrixConsentState | null {
  if (typeof document === 'undefined') return null
  const match = document.cookie.match(new RegExp(`(?:^|; )${CONSENT_COOKIE}=([^;]*)`))
  if (!match) return null
  try {
    return JSON.parse(decodeURIComponent(match[1])) as MatrixConsentState
  } catch {
    return null
  }
}

function writeCookie(consent: MatrixConsentState) {
  const secure = typeof location !== 'undefined' && location.protocol === 'https:' ? '; Secure' : ''
  document.cookie =
    `${CONSENT_COOKIE}=${encodeURIComponent(JSON.stringify(consent))}` +
    `; Max-Age=${CONSENT_MAX_AGE}; Path=/; SameSite=Lax${secure}`
}

export function clearConsentCookie() {
  if (typeof document === 'undefined') return
  document.cookie = `${CONSENT_COOKIE}=; Max-Age=0; Path=/; SameSite=Lax`
}

export function privacySignal(): boolean {
  if (typeof navigator === 'undefined') return false
  try {
    const nav = navigator as Navigator & { globalPrivacyControl?: boolean }
    const win = typeof window !== 'undefined' ? (window as Window & { doNotTrack?: string }) : null
    return nav.globalPrivacyControl === true || nav.doNotTrack === '1' || win?.doNotTrack === '1'
  } catch {
    return false
  }
}

export function notifyConsentListeners(consent: MatrixConsentState) {
  listeners.forEach((fn) => {
    try {
      fn(consent)
    } catch {
      /* ignore */
    }
  })
}

export function saveConsent(choice: MatrixConsentChoice): MatrixConsentState {
  const consent: MatrixConsentState = {
    v: CONSENT_VERSION,
    necessary: true,
    functional: !!choice.functional,
    analytics: !!choice.analytics,
    ts: Date.now(),
  }
  writeCookie(consent)
  notifyConsentListeners(consent)
  return consent
}

export function readStoredConsent(): MatrixConsentState | null {
  const existing = readCookie()
  if (!existing || existing.v !== CONSENT_VERSION) return null
  return existing
}

export function subscribeConsent(fn: ConsentListener): () => void {
  listeners.add(fn)
  const existing = readStoredConsent()
  if (existing) fn(existing)
  return () => listeners.delete(fn)
}

export function registerOpenPreferences(handler: () => void) {
  openPreferencesHandler = handler
}

export function unregisterOpenPreferences() {
  openPreferencesHandler = null
}

export function installMatrixConsentApi() {
  if (typeof window === 'undefined') return

  const api: MatrixConsentApi = {
    get: () => readStoredConsent(),
    has: () => readStoredConsent() !== null,
    allowed: (cat) => {
      const consent = readStoredConsent()
      return !!consent?.[cat]
    },
    openPreferences: () => openPreferencesHandler?.(),
    onChange: (fn) => subscribeConsent(fn),
    reset: () => {
      clearConsentCookie()
      openPreferencesHandler?.()
    },
  }

  window.MatrixConsent = api
}

/** Listen for `[data-cookie-settings]` clicks anywhere in the document. */
export function bindCookieSettingsTriggers(open: () => void) {
  const onClick = (event: MouseEvent) => {
    const target = event.target
    if (!(target instanceof Element)) return
    const trigger = target.closest('[data-cookie-settings]')
    if (!trigger) return
    event.preventDefault()
    open()
  }
  document.addEventListener('click', onClick)
  return () => document.removeEventListener('click', onClick)
}
