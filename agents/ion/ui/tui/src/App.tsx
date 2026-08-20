import {
  Box,
  Text,
  useApp,
  useInput,
  useStdout,
  type Key,
} from 'ink'
import TextInput from 'ink-text-input'
import React, { useEffect, useMemo, useRef, useState } from 'react'
import {
  ControlPlaneClient,
  displayModelCompatibility,
  isComputerEventPayload,
  migrateDisplayModel,
  type CommandDescriptor,
  type EventEnvelope,
  type Operation,
  type OperatorState,
  type ProjectVerificationManifest,
  type ProjectVerificationRun,
  type ProjectVerificationWaiver,
} from '@centra-ai/ion-shared'
import type { LocalControlPlaneTransport } from './local-client.js'

const sections = [
  ['chat', 'Chat'],
  ['tasks', 'Tasks'],
  ['approvals', 'Approvals'],
  ['projects', 'Projects'],
  ['schedules', 'Schedules'],
  ['skills', 'Skills'],
  ['memory', 'Memory'],
  ['computer', 'Computer'],
  ['system', 'System'],
] as const

type Section = (typeof sections)[number][0]

interface AppProps {
  transport: LocalControlPlaneTransport
  state: OperatorState
  connection: 'ready' | 'degraded'
  error?: string
  onOpenEditor?(current: string): Promise<string>
}

interface BrowserWorkflow {
  id: string
  status: string
  origin: string
  revision: number
  handoff?: {
    kind: string
    consequence: string
  }
}

const palette = {
  canvas: '#0b0d0e',
  surface: '#151819',
  raised: '#1d2121',
  selected: '#26302d',
  text: '#edf2ef',
  muted: '#8d9994',
  faint: '#5f6965',
  accent: '#8ad8b6',
  accentInk: '#07110d',
  info: '#8dbbd8',
  warning: '#e5bf73',
  danger: '#ec8f86',
} as const

export function App({
  transport,
  state,
  connection,
  error,
  onOpenEditor,
}: AppProps) {
  const { exit } = useApp()
  const { stdout } = useStdout()
  const width = stdout?.columns ?? 100
  const height = stdout?.rows ?? 30
  const compact = width < 104
  const narrow = width < 72
  const wide = width >= 126
  const noColor = process.env.NO_COLOR !== undefined || process.env.TERM === 'dumb'
  const client = useMemo(
    () => new ControlPlaneClient(transport.actorID, transport),
    [transport],
  )
  const [section, setSection] = useState<Section>('chat')
  const sectionRef = useRef<Section>('chat')
  const [draft, setDraft] = useState('')
  const [sessionID, setSessionID] = useState<string>()
  const [notice, setNotice] = useState('What would you like to accomplish?')
  const [commandOutput, setCommandOutput] = useState('')
  const [catalog, setCatalog] = useState<CommandDescriptor[]>([])
  const [livingState, setLivingState] = useState<Record<string, unknown>>({})
  const [soulState, setSoulState] = useState<Record<string, unknown>>({})
  const [workBrief, setWorkBrief] = useState<Record<string, unknown>>({})
  const [returnBrief, setReturnBrief] = useState<Record<string, unknown>>({})
  const [projectPortfolio, setProjectPortfolio] = useState<Record<string, unknown>>({})
  const [projectVerification, setProjectVerification] = useState<{
    manifest?: ProjectVerificationManifest
    runs: ProjectVerificationRun[]
    waivers: ProjectVerificationWaiver[]
  }>({ runs: [], waivers: [] })
  const [studioState, setStudioState] = useState<Record<string, unknown>>({})
  const [sessions, setSessions] = useState<unknown[]>([])
  const [taskTodos, setTaskTodos] = useState<unknown[]>([])
  const [schedules, setSchedules] = useState<unknown[]>([])
  const [skillState, setSkillState] = useState<Record<string, unknown>>({})
  const [providers, setProviders] = useState<unknown[]>([])
  const [browserWorkflows, setBrowserWorkflows] = useState<BrowserWorkflow[]>([])
  const [sessionCursor, setSessionCursor] = useState(0)
  const [queue, setQueue] = useState<string[]>([])
  const [focus, setFocus] = useState<'nav' | 'composer'>('composer')
  const focusRef = useRef<'nav' | 'composer'>('composer')
  const [busy, setBusy] = useState(false)
  const [computerFollowLive, setComputerFollowLive] = useState(true)
  const [computerCursor, setComputerCursor] = useState(-1)
  const pendingApproval = Object.values(state.pending_approvals)[0]
  const firstProject = arrayField(projectPortfolio, 'projects')[0]
  const activeProjectID = typeof firstProject === 'object' && firstProject !== null && !Array.isArray(firstProject) &&
    typeof (firstProject as Record<string, unknown>).id === 'string'
    ? (firstProject as Record<string, unknown>).id as string
    : undefined
  const recent = state.recent_events
    .filter(isOperatorActivity)
    .slice(-Math.max(4, height - 17))
  const computerEvents = state.recent_events.filter((event) => event.type.startsWith('tool.'))
  const computerIndex = computerFollowLive
    ? computerEvents.length - 1
    : Math.max(0, Math.min(computerCursor, computerEvents.length - 1))
  const computerEvent = computerEvents[computerIndex]
  const runningTurns = Object.values(state.turns).filter(
    (turn) =>
      (turn.status === 'running' || turn.status === 'recovering') &&
      (sessionID === undefined || turn.session_id === sessionID),
  )
  const slashSuggestions = useMemo(() => {
    if (!draft.startsWith('/') || draft.includes(' ')) return []
    const prefix = draft.slice(1).toLowerCase()
    const local = [
      { operation: 'help', description: 'Show slash command help' },
      { operation: 'settings', description: 'Show current settings' },
      { operation: 'new', description: 'Start a new conversation' },
    ]
    const discovered = catalog
      .filter((item) => item.available)
      .map((item) => ({
        operation: item.operation,
        description: item.description,
      }))
    return [...local, ...discovered]
      .filter((item) => item.operation.toLowerCase().startsWith(prefix))
      .slice(0, 7)
  }, [catalog, draft])

  useEffect(() => {
    void client
      .query<CommandDescriptor[]>('commands.catalog', {})
      .then((response) => setCatalog(response.result ?? []))
      .catch((reason: unknown) => setNotice(String(reason)))
  }, [client])

  useEffect(() => {
    const scope = sessionID === undefined ? {} : { session_id: sessionID }
    void client
      .query<unknown[]>('session.list', {})
      .then((response) => setSessions(Array.isArray(response.result) ? response.result : []))
      .catch(() => setSessions([]))
    void client
      .query<Record<string, unknown>>('soul.get', {}, scope)
      .then((response) => setSoulState(response.result ?? {}))
      .catch(() => setSoulState({}))
    void client
      .query<Record<string, unknown>>('work.brief', {}, scope)
      .then((response) => setWorkBrief(response.result ?? {}))
      .catch(() => setWorkBrief({}))
    void client
      .query<Record<string, unknown>>('continuity.brief', { period: '24h' }, scope)
      .then((response) => setReturnBrief(response.result ?? {}))
      .catch(() => setReturnBrief({}))
    void client
      .query<Record<string, unknown>>('project.list', {}, scope)
      .then((response) => setProjectPortfolio(response.result ?? {}))
      .catch(() => setProjectPortfolio({}))
    void client
      .query<Record<string, unknown>>('studio.intent.list', {}, scope)
      .then((response) => setStudioState(response.result ?? {}))
      .catch(() => setStudioState({}))
    void client
      .query<unknown[]>('schedule.list', {}, scope)
      .then((response) => setSchedules(Array.isArray(response.result) ? response.result : []))
      .catch(() => setSchedules([]))
    void client
      .query<Record<string, unknown>>('skill.lifecycle', {}, scope)
      .then((response) => setSkillState(response.result ?? {}))
      .catch(() => setSkillState({}))
    void client
      .query<unknown[]>('provider.list', {}, scope)
      .then((response) => setProviders(Array.isArray(response.result) ? response.result : []))
      .catch(() => setProviders([]))
    if (sessionID !== undefined) {
      void client
        .query<BrowserWorkflow[]>('browser.workflow.list', {}, scope)
        .then((response) => setBrowserWorkflows(Array.isArray(response.result) ? response.result : []))
        .catch(() => setBrowserWorkflows([]))
    } else {
      setBrowserWorkflows([])
    }
    if (sessionID !== undefined) {
      void client
        .query<Record<string, unknown>>('liveness.get', {}, scope)
        .then((response) => setLivingState(response.result ?? {}))
        .catch(() => setLivingState({}))
      void client
        .query<unknown[]>('taskgraph.todo', {}, scope)
        .then((response) => setTaskTodos(Array.isArray(response.result) ? response.result : []))
        .catch(() => setTaskTodos([]))
    } else {
      setTaskTodos([])
    }
  }, [client, sessionID, state.recent_events.length])

  useEffect(() => {
    if (activeProjectID === undefined) {
      setProjectVerification({ runs: [], waivers: [] })
      return
    }
    const scope = sessionID === undefined ? {} : { session_id: sessionID }
    void Promise.all([
      client.query<ProjectVerificationManifest>('project.verification.manifest.get', { project_id: activeProjectID }, scope),
      client.query<ProjectVerificationRun[]>('project.verification.runs', { project_id: activeProjectID }, scope),
      client.query<ProjectVerificationWaiver[]>('project.verification.waivers', { project_id: activeProjectID }, scope),
    ]).then(([manifest, runs, waivers]) => {
      setProjectVerification({
        ...(manifest.result === undefined ? {} : { manifest: manifest.result }),
        runs: Array.isArray(runs.result) ? runs.result : [],
        waivers: Array.isArray(waivers.result) ? waivers.result : [],
      })
    }).catch(() => setProjectVerification({ runs: [], waivers: [] }))
  }, [activeProjectID, client, sessionID, state.recent_events.length])

  useEffect(() => {
    if (computerFollowLive) setComputerCursor(computerEvents.length - 1)
  }, [computerEvents.length, computerFollowLive])

  useEffect(() => {
    setSessionCursor((current) => Math.max(0, Math.min(current, sessions.length - 1)))
  }, [sessions.length])

  const send = async (content: string) => {
    if (content === '' || busy) return
    if (content.startsWith('/')) {
      await executeSlash(content)
      return
    }
    setBusy(true)
    setDraft('')
    try {
      let selected = sessionID
      if (selected === undefined) {
        const created = await client.command<{ id: string }>(
          'session.create',
          {},
          crypto.randomUUID(),
        )
        if (created.error !== undefined || created.result?.id === undefined) {
          throw new Error(created.error?.message ?? 'session creation failed')
        }
        selected = created.result.id
        setSessionID(selected)
      }
      const response = await client.command(
        'turn.submit',
        { content, surface: 'general' },
        crypto.randomUUID(),
        { session_id: selected },
      )
      if (response.error !== undefined) throw new Error(response.error.message)
      setNotice('Request started. Live updates will appear here.')
    } catch (reason) {
      setNotice(reason instanceof Error ? reason.message : String(reason))
    } finally {
      setBusy(false)
    }
  }

  const submit = async (value: string) => {
    const content = value.trim()
    if (content === '') return
    if (busy || runningTurns.length > 0) {
      setQueue((current) => [...current.slice(-19), content])
      setDraft('')
      setNotice('Added to the queue. Ion will start it next.')
      return
    }
    await send(content)
  }

  useEffect(() => {
    if (busy || runningTurns.length > 0 || queue.length === 0) return
    const [next, ...remaining] = queue
    if (next === undefined) return
    setQueue(remaining)
    void send(next)
  }, [busy, queue, runningTurns.length])

  const executeSlash = async (value: string) => {
    setBusy(true)
    setDraft('')
    setCommandOutput('')
    try {
      const separator = value.indexOf(' ')
      const operation = value.slice(1, separator < 0 ? undefined : separator).trim()
      const rawPayload = separator < 0 ? '' : value.slice(separator + 1).trim()
      if (operation === 'help') {
        const available = catalog.filter((item) => item.available)
        setCommandOutput([
          'Slash commands come from the live server catalog.',
          'Run /operation or /operation {"field":"value"}.',
          '/settings shows current settings. /new starts a conversation.',
          '',
          ...available.map(
            (item) => `/${item.operation}  [${item.kind}]  ${item.description}`,
          ),
        ].join('\n'))
        setNotice(`${available.length} server operations available.`)
        return
      }
      if (operation === 'new') {
        if (!catalog.some(
          (item) => item.available && item.operation === 'session.create',
        )) {
          throw new Error('session.create is not available')
        }
        const created = await client.command<{ id: string }>(
          'session.create',
          {},
          crypto.randomUUID(),
        )
        if (created.error !== undefined || created.result?.id === undefined) {
          throw new Error(created.error?.message ?? 'session creation failed')
        }
        setSessionID(created.result.id)
        setNotice('New encrypted conversation ready.')
        return
      }
      const resolvedOperation = operation === 'settings' ? 'config.get' : operation
      const descriptor = catalog.find(
        (item) => item.available && item.operation === resolvedOperation,
      )
      if (descriptor === undefined) {
        throw new Error(`/${operation} is not available; use /help`)
      }
      const catalogOperation = descriptor.operation as Operation
      let payload: Record<string, unknown> = {}
      if (rawPayload !== '') {
        const parsed: unknown = JSON.parse(rawPayload)
        if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
          throw new Error('slash command payload must be a JSON object')
        }
        payload = parsed as Record<string, unknown>
      }
      const scope = sessionID === undefined ? {} : { session_id: sessionID }
      const response = descriptor.kind === 'query'
        ? await client.query(catalogOperation, payload, scope)
        : await client.command(
          catalogOperation,
          payload,
          crypto.randomUUID(),
          scope,
        )
      if (response.error !== undefined) throw new Error(response.error.message)
      setCommandOutput(JSON.stringify(response.result ?? {}, null, 2))
      setNotice(`/${operation} completed.`)
      if (operation === 'settings') setSection('system')
    } catch (reason) {
      setNotice(reason instanceof Error ? reason.message : String(reason))
    } finally {
      setBusy(false)
    }
  }

  const decide = async (decision: 'approve' | 'deny') => {
    if (pendingApproval === undefined) return
    const response = await client.command(
      'approval.respond',
      { approval_id: pendingApproval.id, decision },
      crypto.randomUUID(),
      pendingApproval.session_id === undefined
        ? {}
        : { session_id: pendingApproval.session_id },
    )
    setNotice(response.error?.message ?? `Approval ${decision} recorded.`)
  }

  const resumeSelectedSession = async () => {
    const selected = sessions[sessionCursor]
    if (typeof selected !== 'object' || selected === null || Array.isArray(selected)) return
    const id = (selected as Record<string, unknown>).id
    if (typeof id !== 'string' || id === '') return
    const response = await client.command(
      'session.resume',
      {},
      crypto.randomUUID(),
      { session_id: id },
    )
    if (response.error !== undefined) {
      setNotice(response.error.message)
      return
    }
    setSessionID(id)
    setCommandOutput('')
    sectionRef.current = 'chat'
    setSection('chat')
    setNotice('Encrypted conversation resumed. New messages continue the same task.')
  }

	const decideStudio = async (accept: boolean) => {
	  const pending = pendingStudioProposal(studioState)
	  if (pending === undefined) return
	  const response = await client.command(
	    'studio.proposal.decide',
	    { intent_id: pending.intentID, proposal_id: pending.proposalID, accept, reason: accept ? 'Approved in terminal Studio' : 'Rejected in terminal Studio' },
	    crypto.randomUUID(),
	    sessionID === undefined ? {} : { session_id: sessionID },
	  )
	  setNotice(response.error?.message ?? (accept ? 'Specification accepted.' : 'Specification rejected.'))
	  const refreshed = await client.query<Record<string, unknown>>('studio.intent.list', {}, sessionID === undefined ? {} : { session_id: sessionID })
	  setStudioState(refreshed.result ?? {})
	}

	const applyStudio = async () => {
	  const accepted = acceptedStudioProposal(studioState)
	  if (accepted === undefined) return
	  const response = await client.command(
	    'studio.proposal.apply',
	    { intent_id: accepted.intentID, proposal_id: accepted.proposalID },
	    crypto.randomUUID(),
	    sessionID === undefined ? {} : { session_id: sessionID },
	  )
	  setNotice(response.error?.message ?? 'Accepted specification applied to authoritative KVX.')
	  const refreshed = await client.query<Record<string, unknown>>('studio.intent.list', {}, sessionID === undefined ? {} : { session_id: sessionID })
	  setStudioState(refreshed.result ?? {})
	}

  const runProjectVerification = async () => {
    if (activeProjectID === undefined || projectVerification.manifest === undefined) {
      setNotice('Prepare a verification manifest before running project gates.')
      return
    }
    setBusy(true)
    const scope = sessionID === undefined ? {} : { session_id: sessionID }
    const response = await client.command<ProjectVerificationRun>(
      'project.verification.run',
      {
        project_id: activeProjectID,
        manifest_id: projectVerification.manifest.id,
        full: true,
        max_attempts: 3,
      },
      crypto.randomUUID(),
      scope,
    )
    if (response.error !== undefined) {
      setNotice(response.error.message)
    } else if (response.result !== undefined) {
      setProjectVerification((current) => ({ ...current, runs: [...current.runs, response.result as ProjectVerificationRun] }))
      setCommandOutput(JSON.stringify(response.result, null, 2))
      setNotice(`Verification finished: ${response.result.status.replaceAll('_', ' ')}.`)
    }
    setBusy(false)
  }

  useInput((input: string, key: Key) => {
    if (input === 'q' && focusRef.current === 'nav') exit()
    if (key.escape) {
      const next = focusRef.current === 'composer' ? 'nav' : 'composer'
      focusRef.current = next
      setFocus(next)
    }
    if (key.tab) {
      if (focus === 'composer' && slashSuggestions[0] !== undefined) {
        setDraft(`/${slashSuggestions[0].operation} `)
        return
      }
      const current = sections.findIndex(([id]) => id === section)
      const next = sections[(current + 1) % sections.length]
      if (next !== undefined) {
        sectionRef.current = next[0]
        setSection(next[0])
      }
    }
    if (focusRef.current === 'nav' && input >= '1' && input <= String(sections.length)) {
      const selected = sections[Number(input) - 1]
      if (selected !== undefined) {
        sectionRef.current = selected[0]
        setSection(selected[0])
      }
    }
    if (input === 'a' && pendingApproval !== undefined) void decide('approve')
    if (input === 'd' && pendingApproval !== undefined) void decide('deny')
    if (sectionRef.current === 'projects' && input === 'y') void decideStudio(true)
    if (sectionRef.current === 'projects' && input === 'n') void decideStudio(false)
    if (sectionRef.current === 'projects' && input === 'p') void applyStudio()
    if (sectionRef.current === 'projects' && input === 'v') void runProjectVerification()
    if (sectionRef.current === 'computer' && focusRef.current === 'nav') {
      if (input === 'l') {
        setComputerFollowLive(true)
        setComputerCursor(computerEvents.length - 1)
      }
      if (input === 'p') {
        setComputerFollowLive(false)
        setComputerCursor(computerIndex)
      }
      if (key.upArrow || input === '[') {
        setComputerFollowLive(false)
        setComputerCursor(Math.max(0, computerIndex - 1))
      }
      if (key.downArrow || input === ']') {
        const next = Math.min(computerEvents.length - 1, computerIndex + 1)
        setComputerCursor(next)
        setComputerFollowLive(next === computerEvents.length - 1)
      }
    }
    if (sectionRef.current === 'system' && focusRef.current === 'nav') {
      if (sessions.length > 0 && (key.upArrow || input === '[')) {
        setSessionCursor(Math.max(0, sessionCursor - 1))
      }
      if (sessions.length > 0 && (key.downArrow || input === ']')) {
        setSessionCursor(Math.min(sessions.length - 1, sessionCursor + 1))
      }
      if (input === 'r') void resumeSelectedSession()
    }
    if (key.ctrl && input === 'e' && onOpenEditor !== undefined) {
      void onOpenEditor(draft).then(setDraft)
    }
  })

  const updateDraft = (value: string) => {
    if (
      focusRef.current === 'nav' &&
      value.length === 1 &&
      value >= '1' &&
      value <= String(sections.length)
    ) {
      const selected = sections[Number(value) - 1]
      if (selected !== undefined) {
        sectionRef.current = selected[0]
        setSection(selected[0])
        setDraft('')
      }
      return
    }
    setDraft(value)
  }

  const availableActions = catalog.filter((item) => item.available).length
  const trackedTurns = Object.keys(state.turns).length
  const waitingDecisions = Object.keys(state.pending_approvals).length
  const workspaceHeight = Math.max(10, height - (narrow ? 9 : 7))

  const stage = (
    <Box flexDirection="column" flexGrow={1} paddingX={compact ? 1 : 2}>
      <Box justifyContent="space-between">
        <Box flexDirection="column">
          <Text {...color(noColor, palette.accent)}>WORKSPACE / {section.toUpperCase()}</Text>
          <Text bold>{sectionTitle(section)}</Text>
        </Box>
        {!compact ? (
          <Box flexDirection="column" alignItems="flex-end">
            <Text dimColor>{sectionSubtitle(section, catalog)}</Text>
            <Text dimColor>{sessionID === undefined ? 'No conversation selected' : `Session ${shortID(sessionID)}`}</Text>
          </Box>
        ) : null}
      </Box>
      <Box flexDirection="column" marginTop={1}>
        {section === 'computer' ? (
          <ComputerPane
            event={computerEvent}
            events={computerEvents}
            followLive={computerFollowLive}
            gap={state.gap}
            index={computerIndex}
            noColor={noColor}
            pendingApproval={pendingApproval !== undefined}
            workflows={browserWorkflows}
          />
        ) : null}
        {section === 'projects' && studioEntries(studioState).length > 0 ? (
          <Surface noColor={noColor} tone="raised">
            <Text bold {...color(noColor, palette.accent)}>Specification review</Text>
            <Text wrap="wrap">{studioDetails(studioState)}</Text>
            <Text dimColor>Y accept  N reject  P apply  V verify</Text>
          </Surface>
        ) : null}
        {commandOutput === '' ? null : (
          <Surface noColor={noColor} tone="raised">
            <Text bold {...color(noColor, palette.info)}>Command result</Text>
            <Text wrap="wrap">{commandOutput}</Text>
          </Surface>
        )}
        {section === 'computer' ? null : (
          <SectionPane
            compact={compact}
            noColor={noColor}
            pendingApprovals={Object.values(state.pending_approvals)}
            projects={projectPortfolio}
            projectVerification={projectVerification}
            providers={providers}
            recent={recent}
            returnBrief={returnBrief}
            schedules={schedules}
            section={section}
            sessionCursor={sessionCursor}
            sessions={sessions}
            skillState={skillState}
            taskTodos={taskTodos}
            workBrief={workBrief}
          />
        )}
      </Box>
    </Box>
  )

  return (
    <Box
      flexDirection="column"
      minHeight={height}
      width={width}
      {...(noColor ? {} : { backgroundColor: palette.canvas })}
    >
      <OperatorHeader
        connection={connection}
        noColor={noColor}
        running={runningTurns.length > 0}
        waitingDecisions={waitingDecisions}
      />
      {narrow ? (
        <HorizontalNavigation
          noColor={noColor}
          section={section}
        />
      ) : null}
      <Box minHeight={workspaceHeight} flexGrow={1}>
        {!narrow ? (
          <NavigationRail
            noColor={noColor}
            section={section}
            width={compact ? 20 : 22}
          />
        ) : null}
        {stage}
        {wide ? (
          <ContextRail
            availableActions={availableActions}
            livingState={livingState}
            noColor={noColor}
            pendingApproval={pendingApproval}
            projectPortfolio={projectPortfolio}
            soulState={soulState}
            studioState={studioState}
            trackedTurns={trackedTurns}
            waitingDecisions={waitingDecisions}
            workBrief={workBrief}
          />
        ) : null}
      </Box>
      <ComposerDock
        busy={busy}
        draft={draft}
        focus={focus}
        noColor={noColor}
        onChange={updateDraft}
        onSubmit={(value) => void submit(value)}
        queue={queue}
        suggestions={slashSuggestions}
      />
      <Box
        paddingX={1}
        justifyContent="space-between"
        {...(noColor ? {} : { backgroundColor: palette.canvas })}
      >
        <Text {...color(noColor || error === undefined, palette.danger)}>
          {error ?? notice}
        </Text>
        {wide ? (
          <Text dimColor>
            {focus === 'nav' ? 'Navigation focus' : 'Composer focus'}  Tab views  Esc focus  Ctrl+E editor  q quit
          </Text>
        ) : null}
      </Box>
    </Box>
  )
}

function OperatorHeader({
  connection,
  noColor,
  running,
  waitingDecisions,
}: {
  connection: 'ready' | 'degraded'
  noColor: boolean
  running: boolean
  waitingDecisions: number
}) {
  const state = connection === 'degraded'
    ? 'DEGRADED'
    : running
      ? 'WORKING'
      : 'READY'
  const stateColor = connection === 'degraded'
    ? palette.danger
    : running
      ? palette.warning
      : palette.accent
  return (
    <Box
      paddingX={1}
      justifyContent="space-between"
      {...(noColor ? {} : { backgroundColor: palette.surface })}
    >
      <Box gap={1}>
        <Text bold {...color(noColor, palette.text)}>ION</Text>
        <Text {...color(noColor, palette.faint)}>operator</Text>
      </Box>
      <Box gap={2}>
        {waitingDecisions > 0 ? (
          <Text bold {...color(noColor, palette.warning)}>
            {waitingDecisions} DECISION{waitingDecisions === 1 ? '' : 'S'}
          </Text>
        ) : null}
        <Text bold {...color(noColor, stateColor)}>{state}</Text>
      </Box>
    </Box>
  )
}

function HorizontalNavigation({
  noColor,
  section,
}: {
  noColor: boolean
  section: Section
}) {
  return (
    <Box
      flexDirection="column"
      paddingX={1}
      {...(noColor ? {} : { backgroundColor: palette.surface })}
    >
      {[sections.slice(0, 5), sections.slice(5)].map((row, rowIndex) => (
        <Box key={`nav-row-${rowIndex}`} gap={1}>
          {row.map(([id, label]) => {
            const index = sections.findIndex(([candidate]) => candidate === id)
            return (
              <Text
                bold={section === id}
                inverse={noColor && section === id}
                key={id}
                {...(noColor
                  ? {}
                  : section === id
                    ? { color: palette.accentInk, backgroundColor: palette.accent }
                    : { color: palette.muted })}
              >
                {index + 1} {label}
              </Text>
            )
          })}
        </Box>
      ))}
    </Box>
  )
}

function NavigationRail({
  noColor,
  section,
  width,
}: {
  noColor: boolean
  section: Section
  width: number
}) {
  return (
    <Box
      width={width}
      flexShrink={0}
      flexDirection="column"
      paddingX={1}
      paddingY={1}
      {...(noColor ? {} : { backgroundColor: palette.surface })}
    >
      <Text dimColor>VIEWS</Text>
      <Box flexDirection="column" marginTop={1}>
        {sections.map(([id, label], index) => (
          <Box
            key={id}
            paddingX={1}
            {...(noColor || section !== id ? {} : { backgroundColor: palette.selected })}
          >
            <Text
              bold={section === id}
              inverse={noColor && section === id}
              {...color(noColor, section === id ? palette.accent : palette.muted)}
            >
              {section === id ? '›' : ' '} {index + 1}  {label}
            </Text>
          </Box>
        ))}
      </Box>
      <Box flexGrow={1} />
      <Text dimColor>Esc  switch focus</Text>
      <Text dimColor>Ctrl+E editor</Text>
      <Text dimColor>/     commands</Text>
      <Text dimColor>q     quit</Text>
    </Box>
  )
}

function ContextRail({
  availableActions,
  livingState,
  noColor,
  pendingApproval,
  projectPortfolio,
  soulState,
  studioState,
  trackedTurns,
  waitingDecisions,
  workBrief,
}: {
  availableActions: number
  livingState: Record<string, unknown>
  noColor: boolean
  pendingApproval: unknown
  projectPortfolio: Record<string, unknown>
  soulState: Record<string, unknown>
  studioState: Record<string, unknown>
  trackedTurns: number
  waitingDecisions: number
  workBrief: Record<string, unknown>
}) {
  return (
    <Box
      width={32}
      flexShrink={0}
      flexDirection="column"
      paddingX={1}
      paddingY={1}
      {...(noColor ? {} : { backgroundColor: palette.surface })}
    >
      <Text dimColor>NOW</Text>
      <Box marginTop={1} flexDirection="column">
        <Text bold {...color(noColor, palette.text)}>Current outcome</Text>
        <Text wrap="wrap">{workSummary(workBrief)}</Text>
      </Box>
      <Box marginTop={1} flexDirection="column">
        <Text bold {...color(noColor, palette.text)}>Workspace</Text>
        <Text dimColor>{projectSummary(projectPortfolio)}</Text>
        <Text dimColor>{studioSummary(studioState)}</Text>
      </Box>
      <Box marginTop={1} flexDirection="column">
        <Text bold {...color(noColor, palette.text)}>Runtime</Text>
        <Text dimColor>{trackedTurns} requests retained</Text>
        <Text dimColor>{availableActions} actions ready</Text>
        <Text dimColor>{waitingDecisions} decisions waiting</Text>
      </Box>
      <Box marginTop={1} flexDirection="column">
        <Text bold {...color(noColor, palette.text)}>Continuity</Text>
        <Text dimColor>{livingSummary(livingState)}</Text>
        <Text dimColor>{soulSummary(soulState)}</Text>
      </Box>
      {pendingApproval === undefined ? null : (
        <Box
          flexDirection="column"
          marginTop={1}
          paddingX={1}
          {...(noColor ? {} : { backgroundColor: palette.raised })}
        >
          <Text bold {...color(noColor, palette.warning)}>Decision required</Text>
          <Text wrap="wrap">{compactPayload(
            typeof pendingApproval === 'object' && pendingApproval !== null &&
            'payload' in pendingApproval
              ? (pendingApproval as { payload: unknown }).payload
              : pendingApproval,
          )}</Text>
          <Text dimColor>A approve  D deny</Text>
        </Box>
      )}
    </Box>
  )
}

function ComposerDock({
  busy,
  draft,
  focus,
  noColor,
  onChange,
  onSubmit,
  queue,
  suggestions,
}: {
  busy: boolean
  draft: string
  focus: 'nav' | 'composer'
  noColor: boolean
  onChange(value: string): void
  onSubmit(value: string): void
  queue: string[]
  suggestions: Array<{ operation: string; description: string }>
}) {
  return (
    <Box flexDirection="column">
      {queue.length === 0 ? null : (
        <Box
          paddingX={1}
          gap={1}
          {...(noColor ? {} : { backgroundColor: palette.raised })}
        >
          <Text bold {...color(noColor, palette.warning)}>{queue.length} QUEUED</Text>
          <Text dimColor wrap="truncate">{queue[0]}</Text>
        </Box>
      )}
      {suggestions.length === 0 ? null : (
        <Box
          flexDirection="column"
          paddingX={2}
          {...(noColor ? {} : { backgroundColor: palette.raised })}
        >
          {suggestions.slice(0, 5).map((suggestion, index) => (
            <Text
              key={suggestion.operation}
              dimColor={index !== 0}
              {...color(noColor, index === 0 ? palette.accent : palette.muted)}
            >
              /{suggestion.operation}  {suggestion.description}
            </Text>
          ))}
        </Box>
      )}
      <Box
        paddingX={1}
        {...(noColor ? {} : { backgroundColor: palette.surface })}
      >
        <Text bold {...color(noColor, palette.accent)}>ION  </Text>
        {focus === 'composer' ? (
          <TextInput
            focus
            onChange={onChange}
            onSubmit={onSubmit}
            placeholder={busy ? 'Working. Add a follow-up to the queue…' : 'Ask, direct, or type / for commands…'}
            value={draft}
          />
        ) : (
          <Text dimColor>{draft === '' ? 'Navigation focus' : draft}</Text>
        )}
      </Box>
    </Box>
  )
}

function Surface({
  children,
  noColor,
  tone = 'surface',
}: {
  children: React.ReactNode
  noColor: boolean
  tone?: 'surface' | 'raised' | 'selected'
}) {
  const backgroundColor = {
    surface: palette.surface,
    raised: palette.raised,
    selected: palette.selected,
  }[tone]
  return (
    <Box
      flexDirection="column"
      marginBottom={1}
      paddingX={1}
      {...(noColor ? {} : { backgroundColor })}
    >
      {children}
    </Box>
  )
}

function SectionPane({
  compact,
  noColor,
  pendingApprovals,
  projects,
  projectVerification,
  providers,
  recent,
  returnBrief,
  schedules,
  section,
  sessionCursor,
  sessions,
  skillState,
  taskTodos,
  workBrief,
}: {
  compact: boolean
  noColor: boolean
  pendingApprovals: unknown[]
  projects: Record<string, unknown>
  projectVerification: {
    manifest?: ProjectVerificationManifest
    runs: ProjectVerificationRun[]
    waivers: ProjectVerificationWaiver[]
  }
  providers: unknown[]
  recent: EventEnvelope[]
  returnBrief: Record<string, unknown>
  schedules: unknown[]
  section: Section
  sessionCursor: number
  sessions: unknown[]
  skillState: Record<string, unknown>
  taskTodos: unknown[]
  workBrief: Record<string, unknown>
}) {
  if (section === 'chat') {
    return (
      <Box flexDirection="column">
        {recent.length === 0 ? (
          <Box flexDirection="column" paddingY={1}>
            <Text bold {...color(noColor, palette.text)}>What are we working on?</Text>
            <Text dimColor>
              Start a conversation, continue an existing outcome, or type / to inspect Ion.
            </Text>
          </Box>
        ) : (
          <>
            <Text dimColor>LIVE ACTIVITY</Text>
            <EventRows compact={compact} events={recent} noColor={noColor} />
          </>
        )}
      </Box>
    )
  }
  if (section === 'tasks') {
    const awayRows = returnBriefRows(returnBrief).slice(0, 6)
    return (
      <Box flexDirection="column">
        <Surface noColor={noColor} tone="selected">
          <Text dimColor>CURRENT OUTCOME</Text>
          <Text bold>{workSummary(workBrief)}</Text>
        </Surface>
        <Surface noColor={noColor}>
          <Text dimColor>OPEN WORK</Text>
          {taskTodos.length === 0 ? (
            <Text dimColor>No open subgoals in this conversation.</Text>
          ) : taskTodos.slice(0, 8).map((item, index) => (
            <Text key={`task-${index}`}>
              <Text {...color(noColor, palette.accent)}>{index + 1}</Text>
              {'  '}{recordLine(item, ['title', 'goal', 'status'])}
            </Text>
          ))}
        </Surface>
        <Text dimColor>SINCE YOUR LAST VISIT</Text>
        {awayRows.length === 0 ? (
          <Text dimColor>No verified changes were recorded in the past day.</Text>
        ) : awayRows.map((row, index) => (
          <Text key={`return-${index}`}>{row}</Text>
        ))}
      </Box>
    )
  }
  if (section === 'approvals') {
    return (
      <Box flexDirection="column">
        {pendingApprovals.length === 0 ? (
          <Box paddingY={1} flexDirection="column">
            <Text bold {...color(noColor, palette.accent)}>No decisions waiting</Text>
            <Text dimColor>Ion can continue without operator intervention.</Text>
          </Box>
        ) : pendingApprovals.map((item, index) => (
          <Surface key={`approval-${index}`} noColor={noColor} tone="raised">
            <Text bold {...color(noColor, palette.warning)}>Decision {index + 1}</Text>
            <Text wrap="wrap">{recordLine(item, ['operation', 'consequence', 'expires_at', 'id'])}</Text>
            <Text dimColor>A approve  D deny</Text>
          </Surface>
        ))}
      </Box>
    )
  }
  if (section === 'projects') {
    const items = arrayField(projects, 'projects')
    const latest = projectVerification.runs.at(-1)
    const gates = Array.isArray(projectVerification.manifest?.gates)
      ? projectVerification.manifest.gates
      : []
    const criteria = Array.isArray(projectVerification.manifest?.criteria)
      ? projectVerification.manifest.criteria
      : []
    const activeWaivers = projectVerification.waivers.filter(
      (waiver) => waiver.revoked_at === undefined && new Date(waiver.expires_at).getTime() > Date.now(),
    )
    return (
      <Box flexDirection="column">
        <Text dimColor>SOFTWARE PROJECTS</Text>
        {items.length === 0 ? (
          <Text dimColor>No registered projects.</Text>
        ) : items.map((item, index) => (
          <Surface key={`project-${index}`} noColor={noColor} tone={index === 0 ? 'selected' : 'surface'}>
            <Text bold>{recordLine(item, ['name', 'lifecycle'])}</Text>
            <Text dimColor>{projectLocation(item)}</Text>
          </Surface>
        ))}
        {projectVerification.manifest === undefined ? (
          <Text dimColor>No verification plan is attached to the first project.</Text>
        ) : (
          <Surface noColor={noColor} tone="raised">
            <Text dimColor>VERIFICATION</Text>
            <Text bold {...color(
              noColor,
              latest?.status === 'passed' ? palette.accent : palette.warning,
            )}>
              {latest?.status.replaceAll('_', ' ') ?? 'Not run'}
            </Text>
            <Text>
              {gates.length} gates  {latest?.criteria_covered?.length ?? 0} covered  {latest?.uncovered_criteria?.length ?? criteria.length} uncovered  {activeWaivers.length} waivers
            </Text>
            <Text dimColor>V run all required gates</Text>
          </Surface>
        )}
      </Box>
    )
  }
  if (section === 'schedules') {
    return (
      <Box flexDirection="column">
        <Text dimColor>RECURRING WORK</Text>
        {schedules.length === 0 ? (
          <Text dimColor>No recurring work is configured.</Text>
        ) : schedules.map((item, index) => (
          <Surface key={`schedule-${index}`} noColor={noColor}>
            <Text bold>{recordLine(item, ['name', 'status'])}</Text>
            <Text dimColor>{recordLine(item, ['next_due_at', 'timezone', 'source'])}</Text>
          </Surface>
        ))}
      </Box>
    )
  }
  if (section === 'skills') {
    const rows = skillRows(skillState)
    return (
      <Box flexDirection="column">
        <Text dimColor>CAPABILITIES</Text>
        {rows.length === 0 ? (
          <Text dimColor>No ability lifecycle records are available.</Text>
        ) : rows.slice(0, 12).map((row, index) => (
          <Text key={`skill-${index}`}>{row}</Text>
        ))}
      </Box>
    )
  }
  if (section === 'memory') {
    const events = recent.filter(
      (event) => event.type.startsWith('memory.') || event.type.startsWith('premise.'),
    )
    return (
      <Box flexDirection="column">
        <Text dimColor>KNOWLEDGE USED IN THIS WORKSPACE</Text>
        <EventRows compact={compact} events={events} noColor={noColor} />
      </Box>
    )
  }
  if (section === 'system') {
    return (
      <Box flexDirection="column">
        <Box justifyContent="space-between">
          <Text dimColor>CONVERSATIONS</Text>
          <Text dimColor>{providers.length} provider{providers.length === 1 ? '' : 's'} configured</Text>
        </Box>
        {sessions.slice(0, 8).map((item, index) => (
          <Box
            key={`session-${index}`}
            paddingX={1}
            {...(noColor || index !== sessionCursor ? {} : { backgroundColor: palette.selected })}
          >
            <Text
              bold={index === sessionCursor}
              inverse={noColor && index === sessionCursor}
              {...color(noColor, index === sessionCursor ? palette.accent : palette.text)}
            >
              {index === sessionCursor ? '›' : ' '} {recordLine(item, ['title', 'preview', 'updated_at'])}
            </Text>
          </Box>
        ))}
        <Text dimColor>[ and ] select  R resume</Text>
        <Box marginTop={1}><Text dimColor>RECENT SYSTEM ACTIVITY</Text></Box>
        <EventRows compact={compact} events={recent.slice(-6)} noColor={noColor} />
      </Box>
    )
  }
  return <EventRows compact={compact} events={recent} noColor={noColor} />
}

function EventRows({
  compact,
  events,
  noColor,
}: {
  compact: boolean
  events: EventEnvelope[]
  noColor: boolean
}) {
  if (events.length === 0) return <Text dimColor>No activity here yet.</Text>
  return (
    <Box flexDirection="column" marginTop={events.length > 0 ? 1 : 0}>
      {events.map((event) => (
        <Box
          key={event.event_id}
          paddingX={1}
          {...(noColor || !event.type.endsWith('.failed')
            ? {}
            : { backgroundColor: palette.raised })}
        >
          <Box width={10} flexShrink={0}>
            <Text dimColor wrap="truncate">{formatTime(event.occurred_at)}</Text>
          </Box>
          <Box width={compact ? undefined : 25} flexShrink={0}>
            <Text bold wrap="truncate" {...color(noColor, eventColor(event.type))}>
              {eventTitle(event.type)}
            </Text>
          </Box>
          {!compact ? (
            <Box flexGrow={1}>
              <Text dimColor wrap="truncate">{eventSummary(event.type, event.payload)}</Text>
            </Box>
          ) : null}
        </Box>
      ))}
    </Box>
  )
}

function arrayField(value: Record<string, unknown>, key: string): unknown[] {
  return Array.isArray(value[key]) ? value[key] : []
}

function recordLine(value: unknown, keys: string[]): string {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return compactPayload(value)
  }
  const record = value as Record<string, unknown>
  const parts = keys.flatMap((key) => {
    const field = record[key]
    return typeof field === 'string' && field !== '' ? [`${key.replaceAll('_', ' ')}: ${field}`] : []
  })
  return parts.length === 0 ? compactPayload(value) : parts.join(' · ')
}

function skillRows(value: Record<string, unknown>): string[] {
  const rows: string[] = []
  for (const status of ['imported', 'active', 'proposed', 'adopted', 'rejected', 'candidates']) {
    const items = value[status]
    if (!Array.isArray(items)) continue
    for (const item of items) {
      rows.push(`${status}: ${recordLine(item, ['name', 'skill_name', 'status', 'revision'])}`)
    }
  }
  if (rows.length === 0 && Object.keys(value).length > 0) rows.push(compactPayload(value))
  return rows
}

function returnBriefRows(value: Record<string, unknown>): string[] {
  const sections = value.sections
  if (!Array.isArray(sections)) return []
  const rows: string[] = []
  for (const section of sections) {
    if (typeof section !== 'object' || section === null || Array.isArray(section)) continue
    const sectionRecord = section as Record<string, unknown>
    if (!Array.isArray(sectionRecord.items)) continue
    const label = typeof sectionRecord.label === 'string' ? sectionRecord.label : 'Update'
    for (const item of sectionRecord.items) {
      if (typeof item !== 'object' || item === null || Array.isArray(item)) continue
      const itemRecord = item as Record<string, unknown>
      if (typeof itemRecord.summary !== 'string') continue
      rows.push(`${label}: ${itemRecord.summary}`)
    }
  }
  return rows
}

function ComputerPane({
  event,
  events,
  followLive,
  gap,
  index,
  noColor,
  pendingApproval,
  workflows,
}: {
  event: EventEnvelope | undefined
  events: EventEnvelope[]
  followLive: boolean
  gap: boolean
  index: number
  noColor: boolean
  pendingApproval: boolean
  workflows: BrowserWorkflow[]
}) {
  if (event === undefined) {
    return (
      <Box flexDirection="column">
        <Text bold {...color(noColor, palette.accent)}>Ready and following live</Text>
        <Text dimColor>No computer activity yet. Actions will appear here with their source and status.</Text>
        <WorkflowSummary noColor={noColor} workflows={workflows} />
      </Box>
    )
  }
  if (!isComputerEventPayload(event.payload)) {
    return (
      <Box flexDirection="column">
        <Text bold {...color(noColor, palette.warning)}>Unsupported retained activity</Text>
        <Text dimColor>Lifecycle version cannot be displayed. No result has been inferred.</Text>
        <Text>Replay event {event.sequence}</Text>
        <WorkflowSummary noColor={noColor} workflows={workflows} />
      </Box>
    )
  }
  const payload = event.payload
  const compatibility = payload.display_model === undefined
    ? undefined
    : displayModelCompatibility(payload.display_model, payload.source_references.length)
  const model = compatibility === 'current' || compatibility === 'migrated'
    ? migrateDisplayModel(payload.display_model, payload.source_references.length)
    : undefined
  const start = Math.max(0, Math.min(events.length - 5, index - 2))
  const visible = events.slice(start, start + 5)
  return (
    <Box flexDirection="column">
      <Box justifyContent="space-between">
        <Text bold {...color(noColor, followLive ? palette.accent : palette.warning)}>
          {followLive ? 'Following live' : 'Inspecting history'}
        </Text>
        <Text>{index + 1}/{events.length} retained events</Text>
      </Box>
      <Surface noColor={noColor} tone="selected">
        <Text dimColor>ACTIVE ACTION</Text>
        <Text bold>{payload.operation.replaceAll('_', ' ')}</Text>
        <Text>Status     {payload.phase.replaceAll('_', ' ')}</Text>
        <Text>Evidence   {model?.title.truth ?? 'lifecycle only'}</Text>
        <Text>
          Sources    {payload.source_references
            .map((source, sourceIndex) => `${source.kind.replaceAll('_', ' ')} ${sourceIndex + 1}`)
            .join(', ')}
        </Text>
        <Text>Surface    {payload.display_kind.replaceAll('_', ' ')}</Text>
        <Text>Artifact   {model?.title.value ?? (payload.result?.available === true ? 'available' : 'not available')}</Text>
        <Text>Approval   {payload.phase === 'awaiting_approval' || pendingApproval ? 'decision waiting' : 'none waiting'}</Text>
        <Text>Replay     event {event.sequence}{gap ? ' · earlier retention gap' : ' · continuous retained window'}</Text>
        {compatibility === 'unsupported' ? (
          <Text>Display    unsupported version; lifecycle remains authoritative</Text>
        ) : null}
      </Surface>
      <WorkflowSummary noColor={noColor} workflows={workflows} />
      <Box flexDirection="column" marginTop={1}>
        <Text dimColor>TIMELINE</Text>
        {visible.map((item, visibleIndex) => (
          <Box
            paddingX={1}
            {...(noColor || item.event_id !== event.event_id
              ? {}
              : { backgroundColor: palette.selected })}
            key={item.event_id}
          >
            <Text
              bold={item.event_id === event.event_id}
              inverse={noColor && item.event_id === event.event_id}
            >
              {start + visibleIndex + 1}  {eventSummary(item.type, item.payload)}
            </Text>
          </Box>
        ))}
      </Box>
    </Box>
  )
}

function WorkflowSummary({
  noColor,
  workflows,
}: {
  noColor: boolean
  workflows: BrowserWorkflow[]
}) {
  return (
    <Box flexDirection="column" marginTop={1}>
      <Text dimColor>SUPERVISED BROWSER</Text>
      {workflows.length === 0 ? (
        <Text dimColor>No active or retained browser workflows.</Text>
      ) : workflows.slice(0, 5).map((workflow) => (
        <Text key={workflow.id}>
          <Text bold {...color(
              noColor,
              workflow.status === 'waiting_for_human' ? palette.warning : palette.info,
            )}>
            {workflow.status.replaceAll('_', ' ')}
          </Text>
          {'  '}
          <Text>
            {workflow.origin} · rev {workflow.revision}
            {workflow.handoff === undefined
              ? ''
              : ` · ${workflow.handoff.kind.replaceAll('_', ' ')}: ${workflow.handoff.consequence}`}
          </Text>
        </Text>
      ))}
    </Box>
  )
}

function sectionTitle(section: Section): string {
  return {
    chat: 'Conversation',
    tasks: 'Goals and active work',
    approvals: 'Decisions',
    projects: 'Projects and Studio',
    schedules: 'Recurring work',
    skills: 'Capabilities',
    memory: 'Knowledge and sources',
    computer: 'Computer activity',
    system: 'System and conversations',
  }[section]
}

function sectionSubtitle(section: Section, catalog: CommandDescriptor[]): string {
  if (section === 'computer') {
    return 'Durable action history  [ ] inspect  P pause  L live'
  }
  const prefixes: Record<Section, string[]> = {
    chat: ['turn.', 'session.'],
    tasks: ['taskgraph.', 'work.'],
    approvals: ['approval.'],
    projects: ['project.', 'studio.'],
    schedules: ['schedule.'],
    skills: ['skill.'],
    memory: ['memory.'],
    computer: ['computer.'],
    system: ['system.', 'provider.', 'channel.', 'policy.', 'logs.'],
  }
  const count = catalog.filter(
    (descriptor) => descriptor.available &&
      prefixes[section].some((prefix) => descriptor.operation.startsWith(prefix)),
  ).length
  const descriptions: Record<Section, string> = {
    chat: 'Encrypted conversation and live work',
    tasks: 'Outcomes, next actions, and verified progress',
    approvals: 'Consequential actions waiting for you',
    projects: 'Software state, specifications, and verification',
    schedules: 'Durable recurring work',
    skills: 'Loaded and evolving abilities',
    memory: 'Evidence-backed context used by Ion',
    computer: 'Durable action history',
    system: 'Sessions, providers, channels, and diagnostics',
  }
  return `${descriptions[section]}  ${count} actions`
}

function compactPayload(value: unknown): string {
  const encoded = JSON.stringify(value)
  return encoded.length > 96 ? `${encoded.slice(0, 93)}…` : encoded
}

function livingSummary(value: Record<string, unknown>): string {
  const relationships = Array.isArray(value.relationships) ? value.relationships.length : 0
  const signals =
    typeof value.signals === 'object' && value.signals !== null
      ? (value.signals as Record<string, unknown>)
      : {}
  const active = signals.task_active === true ||
    (typeof signals.task_duration === 'number' && signals.task_duration > 0)
  return `${relationships} relationship contexts · ${active ? 'task active' : 'task inactive'}`
}

function soulSummary(value: Record<string, unknown>): string {
  const current =
    typeof value.current === 'object' && value.current !== null
      ? (value.current as Record<string, unknown>)
      : {}
  const version = current.number
  const proposals = Array.isArray(value.pending_proposals) ? value.pending_proposals.length : 0
  return `SOUL v${typeof version === 'number' ? version : '—'} · ${proposals} proposals pending`
}

function workSummary(value: Record<string, unknown>): string {
	const contract = typeof value.contract === 'object' && value.contract !== null
	  ? value.contract as Record<string, unknown>
	  : undefined
	if (contract === undefined) return 'No outcome contract yet'
	const goal = typeof contract.goal === 'string' ? contract.goal : 'Current outcome'
	const next = typeof value.next_action === 'string' ? value.next_action : 'choose next action'
	const percent = typeof value.completion_percentage === 'number' ? value.completion_percentage : 0
	const summary = `${goal} · ${percent}% verified · next: ${next}`
	return summary.length > 120 ? `${summary.slice(0, 117)}…` : summary
}

function projectSummary(value: Record<string, unknown>): string {
	const projects = Array.isArray(value.projects) ? value.projects : []
	const ready = projects.filter((project) =>
		typeof project === 'object' && project !== null &&
		(project as Record<string, unknown>).lifecycle === 'ready'
	).length
	return `${projects.length} software projects · ${ready} ready`
}

function studioEntries(value: Record<string, unknown>): Array<Record<string, unknown>> {
	return Array.isArray(value.intents)
	  ? value.intents.filter((item): item is Record<string, unknown> => typeof item === 'object' && item !== null && !Array.isArray(item))
	  : []
}

function pendingStudioProposal(value: Record<string, unknown>): { intentID: string; proposalID: string } | undefined {
	for (const intent of studioEntries(value)) {
	  if (typeof intent.id !== 'string' || !Array.isArray(intent.proposals)) continue
	  for (const proposal of intent.proposals) {
	    if (typeof proposal === 'object' && proposal !== null && !Array.isArray(proposal)) {
	      const record = proposal as Record<string, unknown>
	      if (record.status === 'proposed' && typeof record.id === 'string') return { intentID: intent.id, proposalID: record.id }
	    }
	  }
	}
	return undefined
}

function acceptedStudioProposal(value: Record<string, unknown>): { intentID: string; proposalID: string } | undefined {
	for (const intent of studioEntries(value)) {
	  if (typeof intent.id !== 'string' || !Array.isArray(intent.proposals)) continue
	  for (const proposal of intent.proposals) {
	    if (typeof proposal === 'object' && proposal !== null && !Array.isArray(proposal)) {
	      const record = proposal as Record<string, unknown>
	      if (record.status === 'accepted' && record.applied_at === undefined && typeof record.id === 'string') {
	        return { intentID: intent.id, proposalID: record.id }
	      }
	    }
	  }
	}
	return undefined
}

function studioSummary(value: Record<string, unknown>): string {
	const intents = studioEntries(value)
	const pending = pendingStudioProposal(value)
	const accepted = acceptedStudioProposal(value)
	if (intents.length === 0) return 'No software specification is waiting'
	const latest = intents[0]
	const goal = typeof latest?.goal === 'string' ? latest.goal : 'Software change'
	const state = pending !== undefined ? 'decision needed' : accepted !== undefined ? 'accepted, ready to apply' : 'reviewed'
	return `${goal} · ${state}`
}

function studioDetails(value: Record<string, unknown>): string {
	const intents = studioEntries(value)
	if (intents.length === 0) return 'No software specification is waiting. Tell Ion what you want to build or change.'
	const intent = intents[0]
	const lines = [typeof intent?.goal === 'string' ? intent.goal : 'Software change']
	if (Array.isArray(intent?.assumptions)) {
	  for (const item of intent.assumptions.slice(0, 2)) {
	    if (typeof item !== 'object' || item === null || Array.isArray(item)) continue
	    const assumption = item as Record<string, unknown>
	    lines.push(singleLine(`${assumption.material === true ? 'Decision needed' : 'Assumption'}: ${String(assumption.statement ?? '')}`))
	    if (assumption.material === true && typeof assumption.decision_needed === 'string') {
	      lines.push(singleLine(`Decision: ${assumption.decision_needed}`))
	    }
	  }
	}
	if (Array.isArray(intent?.proposals)) {
	  const proposal = intent.proposals.find((item) => typeof item === 'object' && item !== null && !Array.isArray(item))
	  if (typeof proposal === 'object' && proposal !== null && !Array.isArray(proposal)) {
	    const record = proposal as Record<string, unknown>
	    lines.push(`Specification v${String(record.version ?? '—')} · ${String(record.status ?? 'proposed')}`)
	    const delta = typeof record.delta === 'object' && record.delta !== null && !Array.isArray(record.delta)
	      ? record.delta as Record<string, unknown> : {}
	    const changes = Array.isArray(delta.user_visible_behavior) ? delta.user_visible_behavior.length : 0
	    const criteria = Array.isArray(delta.acceptance_criteria) ? delta.acceptance_criteria.length : 0
	    if (changes > 0 || criteria > 0) {
	      lines.push(`${changes} visible changes · ${criteria} acceptance criteria`)
	    }
	  }
	}
	return lines.join('\n')
}

function projectLocation(value: unknown): string {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return compactPayload(value)
  const record = value as Record<string, unknown>
  const root = typeof record.root === 'string' ? record.root : ''
  const folder = root.split('/').filter(Boolean).at(-1)
  const id = typeof record.id === 'string' ? shortID(record.id) : ''
  if (folder !== undefined && id !== '') return `${folder} · ${id}`
  if (folder !== undefined) return folder
  if (id !== '') return id
  return 'Workspace location unavailable'
}

function eventSummary(type: string, value: unknown): string {
  if (type.startsWith('tool.')) {
    if (!isComputerEventPayload(value)) return 'Retained tool activity uses an unsupported version'
    if (value.display_model !== undefined) {
      const compatibility = displayModelCompatibility(
        value.display_model,
        value.source_references.length,
      )
      if (compatibility === 'unsupported') return 'Display version is not supported'
      const model = migrateDisplayModel(
        value.display_model,
        value.source_references.length,
      )
      if (model !== undefined) {
        return singleLine(`${model.title.value} · ${model.title.truth}`)
      }
    }
    return singleLine(`${value.tool.replaceAll('_', ' ')} · ${value.phase}`)
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    return value === undefined || value === null ? 'Details recorded' : singleLine(String(value))
  }
  const record = value as Record<string, unknown>
  for (const key of ['content', 'message', 'consequence', 'status', 'name']) {
    const detail = record[key]
    if (typeof detail === 'string' && detail !== '') {
      return singleLine(detail)
    }
  }
  return 'Details recorded'
}

function singleLine(value: string): string {
  const normalized = value.replace(/\s+/g, ' ').trim()
  return normalized.length > 96 ? `${normalized.slice(0, 93)}…` : normalized
}

function eventTitle(type: string): string {
  const labels: Record<string, string> = {
    'agent.started': 'Helper started',
    'agent.completed': 'Helper finished',
    'approval.requested': 'Decision needed',
    'approval.responded': 'Decision recorded',
    'memory.activated': 'Saved knowledge used',
    'policy.allowed': 'Safety checks passed',
    'policy.denied': 'Action blocked',
    'prediction.created': 'Forecast recorded',
    'premise.created': 'Assumption recorded',
    'reasoning.summary': 'Reasoning summary',
    'task.completed': 'Task finished',
    'task.started': 'Task started',
    'tool.completed': 'Action finished',
    'tool.failed': 'Action failed',
    'tool.started': 'Action started',
    'turn.completed': 'Response complete',
	'turn.incomplete': 'Work paused with progress saved',
	'turn.recovery': 'Work recovery started',
    'turn.failed': 'Request failed',
    'turn.started': 'Request started',
	'workspace.operation.queued': 'Workspace operation queued',
	'workspace.operation.started': 'Workspace operation started',
	'workspace.operation.progress': 'Workspace operation progressing',
	'workspace.operation.completed': 'Workspace operation completed',
	'workspace.operation.failed': 'Workspace operation failed',
	'workspace.operation.cancelled': 'Workspace operation cancelled',
  }
  return labels[type] ?? type.split('.').map(humanize).join(' · ')
}

function isOperatorActivity(event: EventEnvelope): boolean {
  if (event.type === 'reasoning.summary' || event.type === 'policy.decision') {
    return false
  }
  return ![
    'emotional.',
    'liveness.',
    'relationship.',
    'temporal.',
  ].some((prefix) => event.type.startsWith(prefix))
}

function humanize(value: string): string {
  const spaced = value.replaceAll('_', ' ')
  return spaced.charAt(0).toUpperCase() + spaced.slice(1)
}

function eventColor(type: string): string {
  if (type.startsWith('approval.') || type.startsWith('policy.')) return palette.warning
  if (type.startsWith('security.') || type.endsWith('.failed')) return palette.danger
  if (type.startsWith('memory.') || type.startsWith('premise.')) return palette.info
  if (type.endsWith('.completed')) return palette.accent
  return palette.text
}

function color(disabled: boolean, value: string): {} | { color: string } {
  return disabled ? {} : { color: value }
}

function formatTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '--:--:--'
  return date.toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
}

function shortID(value: string): string {
  return value.length <= 8 ? value : value.slice(0, 8)
}
