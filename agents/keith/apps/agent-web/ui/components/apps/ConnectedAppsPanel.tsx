'use client'

import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react'

import { CheckCircle, Refresh, Search, Stop, Tools, Warning, X } from '@/components/icons'
import {
  connectedAppsCommand,
  connectedAppsData,
  type ConnectedAppAccountProjection,
  type ConnectedAppsIntent,
  type ConnectedAppsProjection,
} from '@/lib/apps'
import type { Command, CommandResult } from '@/lib/keith'

export function ConnectedAppsPanel({
  profileId,
  onCommand,
}: {
  profileId: string | null
  onCommand: (command: Command) => Promise<CommandResult | null>
}) {
  const [projection, setProjection] = useState<ConnectedAppsProjection | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [safeError, setSafeError] = useState<string | null>(null)
  const [authLink, setAuthLink] = useState<string | null>(null)
  const [auditCorrelation, setAuditCorrelation] = useState<string | null>(null)
  const [safeResult, setSafeResult] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)
  const [query, setQuery] = useState('')

  const run = useCallback(
    async (intent: ConnectedAppsIntent, key: string = intent.action) => {
      if (!profileId) return
      setBusy(key)
      setSafeError(null)
      try {
        const result = await onCommand(connectedAppsCommand(profileId, intent))
        if (!result) throw new Error('Keith did not return an authoritative Apps response.')
        if (result.result.status === 'rejected') {
          throw new Error(
            result.result.payload.error?.safe_message ??
              result.result.payload.error?.message ??
              'Keith rejected the connected-app request.',
          )
        }
        const data = connectedAppsData(result)
        if (!data) throw new Error('Keith returned an invalid Apps response.')
        setProjection(data.projection)
        setAuthLink(data.auth_link?.redirect_url ?? null)
        setAuditCorrelation(data.audit_correlation ?? null)
        setSafeResult(data.safe_result === undefined ? null : JSON.stringify(data.safe_result).slice(0, 2_000))
      } catch (error) {
        setSafeError(error instanceof Error ? error.message : 'Keith could not update Apps.')
      } finally {
        setBusy(null)
      }
    },
    [onCommand, profileId],
  )

  useEffect(() => {
    if (profileId) void run({ action: 'browse' })
  }, [profileId, run])

  const toolkits = useMemo(() => {
    const names = new Set<string>()
    projection?.accounts.forEach((account) => names.add(account.toolkit))
    projection?.allowed_tools.forEach((tool) => names.add(tool.toolkit))
    return [...names].sort()
  }, [projection])
  const visibleToolkits = toolkits.filter((toolkit) =>
    toolkit.toLowerCase().includes(query.trim().toLowerCase()),
  )

  const connect = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const toolkit = String(form.get('toolkit') ?? '').trim().toLowerCase()
    const authConfigId = String(form.get('auth_config_id') ?? '').trim()
    if (!toolkit || !authConfigId) return
    void run({ action: 'connect', toolkit, auth_config_id: authConfigId }, `connect:${toolkit}`)
  }

  if (!profileId) {
    return <EmptyApps message="Choose a Keith profile before connecting an app." />
  }

  return (
    <section className="connected-apps" aria-label="Connected Apps">
      <div className="apps-toolbar">
        <label className="apps-search">
          <Search size={16} />
          <span className="sr-only">Filter toolkits</span>
          <input
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Filter apps"
          />
        </label>
        <button
          className="secondary-button"
          disabled={busy !== null}
          onClick={() => void run({ action: 'browse' })}
        >
          <Refresh size={16} /> Refresh
        </button>
      </div>

      <form className="apps-connect panel-form compact" onSubmit={connect}>
        <label>
          Toolkit
          <input name="toolkit" required placeholder="gmail" autoComplete="off" />
        </label>
        <label>
          Auth configuration
          <input name="auth_config_id" required placeholder="gmail-oauth" autoComplete="off" />
        </label>
        <button className="primary-button" type="submit" disabled={busy !== null}>
          <Tools size={16} /> Connect app
        </button>
      </form>

      {authLink ? (
        <div className="apps-notice" role="status">
          <CheckCircle size={17} />
          <span>Authorization is ready in a short-lived provider window.</span>
          <a href={authLink} target="_blank" rel="noreferrer">Continue</a>
          <button className="icon-button" aria-label="Dismiss authorization link" onClick={() => setAuthLink(null)}><X size={15} /></button>
        </div>
      ) : null}
      {safeError ? <p className="form-error" role="alert"><Warning size={16} /> {safeError}</p> : null}
      <div className="apps-session" aria-live="polite">
        <span className={`status-dot ${projection?.session?.state === 'active' ? 'running' : ''}`} />
        <span>{projection?.session ? `Hosted tools ${projection.session.state}` : 'Hosted tools not started'}</span>
        {auditCorrelation ? <code title="Audit correlation">{auditCorrelation}</code> : null}
      </div>
      {safeResult ? <pre className="apps-safe-result" aria-label="Latest safe app result">{safeResult}</pre> : null}

      {!projection && busy ? <p className="muted">Loading connected apps…</p> : null}
      {projection && visibleToolkits.length === 0 ? (
        <EmptyApps message={query ? 'No apps match this filter.' : 'No app toolkits are configured yet.'} />
      ) : null}
      <div className="apps-grid">
        {visibleToolkits.map((toolkit) => {
          const accounts = projection?.accounts.filter((account) => account.toolkit === toolkit) ?? []
          const tools = projection?.allowed_tools.filter((tool) => tool.toolkit === toolkit) ?? []
          return (
            <article className="app-card" key={toolkit}>
              <header>
                <span className="app-mark" aria-hidden="true">{toolkit.slice(0, 2).toUpperCase()}</span>
                <div><h3>{toolkit}</h3><p>{tools.length} allowed tool{tools.length === 1 ? '' : 's'}</p></div>
              </header>
              {tools.length ? <ul className="app-tool-list">{tools.map((tool) => <li key={tool.tool}><code>{tool.tool}</code><span>{friendlyRisk(tool.risk)}</span></li>)}</ul> : <p className="muted">No tools allowed. Keith cannot invoke this toolkit.</p>}
              <ToolAllowlistEditor toolkit={toolkit} tools={tools.map((tool) => tool.tool)} busy={busy !== null} onRun={run} />
              <div className="app-accounts">
                {accounts.length === 0 ? <p className="muted">Not connected</p> : accounts.map((account) => (
                  <AccountRow
                    key={account.id}
                    account={account}
                    busy={busy}
                    confirmingDelete={confirmDelete === account.id}
                    onConfirmDelete={setConfirmDelete}
                    onRun={run}
                  />
                ))}
              </div>
            </article>
          )
        })}
      </div>
    </section>
  )
}

function AccountRow({
  account,
  busy,
  confirmingDelete,
  onConfirmDelete,
  onRun,
}: {
  account: ConnectedAppAccountProjection
  busy: string | null
  confirmingDelete: boolean
  onConfirmDelete: (id: string | null) => void
  onRun: (intent: ConnectedAppsIntent, key?: string) => Promise<void>
}) {
  const blocked = busy !== null
  return (
    <div className="app-account">
      <div className="app-account-heading">
        <div><strong>{account.account_identity}</strong><small>{account.granted_scopes.join(', ') || 'No scopes reported'}</small></div>
        <span className={`status-badge ${account.state === 'active' ? 'enabled' : ''}`}>{account.state}</span>
      </div>
      {account.safe_error ? <p className="form-error">{account.safe_error}</p> : null}
      <div className="app-account-actions">
        {account.state === 'connecting' ? <button disabled={blocked} onClick={() => void onRun({ action: 'complete_callback', account_id: account.id }, `callback:${account.id}`)}>Complete connection</button> : null}
        <button disabled={blocked} onClick={() => void onRun({ action: 'test', account_id: account.id }, `test:${account.id}`)}>Test</button>
        <button disabled={blocked || account.state !== 'active'} onClick={() => void onRun({ action: 'select', account_id: account.id }, `select:${account.id}`)}>Select</button>
        {account.state === 'disabled' || account.state === 'expired' ? (
          <button disabled={blocked} onClick={() => void onRun({ action: 'resume', account_id: account.id }, `resume:${account.id}`)}>Resume</button>
        ) : (
          <button disabled={blocked || account.state !== 'active'} onClick={() => void onRun({ action: 'disable', account_id: account.id }, `disable:${account.id}`)}>Disable</button>
        )}
        <button disabled={blocked || account.state === 'revoked'} onClick={() => void onRun({ action: 'revoke', account_id: account.id }, `revoke:${account.id}`)}><Stop size={13} /> Revoke</button>
        {!confirmingDelete ? (
          <button disabled={blocked || account.state !== 'revoked'} onClick={() => onConfirmDelete(account.id)}>Delete</button>
        ) : (
          <><button className="danger-button" disabled={blocked} onClick={() => { onConfirmDelete(null); void onRun({ action: 'delete', account_id: account.id }, `delete:${account.id}`) }}>Confirm delete</button><button disabled={blocked} onClick={() => onConfirmDelete(null)}>Cancel</button></>
        )}
      </div>
    </div>
  )
}

function ToolAllowlistEditor({
  toolkit,
  tools,
  busy,
  onRun,
}: {
  toolkit: string
  tools: string[]
  busy: boolean
  onRun: (intent: ConnectedAppsIntent, key?: string) => Promise<void>
}) {
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const data = new FormData(event.currentTarget)
    const next = String(data.get('tools') ?? '')
      .split(',')
      .map((tool) => tool.trim().toUpperCase())
      .filter(Boolean)
    void onRun({ action: 'set_tools', toolkit, tools: [...new Set(next)] }, `tools:${toolkit}`)
  }
  return (
    <form className="app-tools-form" onSubmit={submit}>
      <label>
        Allowed tools
        <input name="tools" defaultValue={tools.join(', ')} placeholder="GMAIL_FETCH_EMAILS" />
      </label>
      <button disabled={busy} type="submit">Save allowlist</button>
    </form>
  )
}

function EmptyApps({ message }: { message: string }) {
  return <div className="apps-empty"><Tools size={22} /><p>{message}</p></div>
}

function friendlyRisk(value: string): string {
  return value.replaceAll('_', ' ')
}
