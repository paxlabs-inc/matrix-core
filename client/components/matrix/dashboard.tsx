'use client'

/**
 * Dashboard — mounts the Construct OS Shell at the route root (frame inversion).
 *
 * Previously the chat surface (NeoSurface) WAS the page and Neo's work opened in
 * a panel beside it. The shell inverts that frame (design "Frame inversion",
 * R3.1/R3.2/R3.5): the ENVIRONMENT is the page, and the conversation with Neo is
 * exactly one panel occupying the `narration` region within it. The existing
 * chat component is REUSED untouched — it is simply mounted as the narration
 * panel's content rather than as the containing page.
 *
 * Auth, provisioning, locale, and the query client are unchanged: this component
 * still sits inside `ProvisioningGate` and the app providers; only WHAT mounts at
 * the root and WHERE the conversation sits has changed. The live feed → shared
 * model wiring (task 8.1) is now in place: `useSurfaceFeed` hydrates the
 * conversation's durable surface record on a cold open and tails the live stream,
 * so the environment reappears as the user left it on reload and the home/activity
 * surfaces survive it (R1.2/R17.1/R17.5).
 */
import dynamic from 'next/dynamic'
import { useState } from 'react'
import { Toaster } from '@/components/ui/sonner'
import { NeoChatSearch } from '@/components/matrix/neo/neo-chat-search'
import { SettingsSheet } from '@/components/matrix/settings-sheet'
import { PanelErrorBoundary } from '@/components/matrix/panel-error-boundary'
import { PanelSkeleton } from '@/components/matrix/panel-skeleton'
import { Shell } from '@/components/matrix/shell/shell'
import { useSurfaceFeed } from '@/lib/construct/use-surface-feed'
import { useDescent } from '@/lib/construct/use-descent'
import { useChat, EMPTY_TASK } from '@/hooks/api/useChat'
import { useHandoffAsk } from '@/components/matrix/finance/finance-composer'

const NeoSurface = dynamic(
  () => import('@/components/matrix/neo/neo-surface').then((m) => ({ default: m.NeoSurface })),
  { loading: () => <PanelSkeleton /> },
)

// "Neo's Computer" — the legacy NeoStep work pane. It is NOT gone: it relocated
// from a squished column beside the chat to the shell's environment stage, where
// it is the big centerpiece. It renders ONLY the legacy work (terminal/browser/
// sources/agents/media); the typed Construct surfaces are rendered by the
// environment-stage feed, so the two never double up.
const NeoComputer = dynamic(
  () => import('@/components/matrix/neo/neo-computer').then((m) => ({ default: m.NeoComputer })),
  { ssr: false },
)

// Timeline (Neo's exposed, read-only memory) and Workspace (files) are
// full-page overlays opened from the sidebar. Loaded on demand so they never
// weigh on the first paint of the chat surface.
const NeoTimeline = dynamic(
  () => import('@/components/matrix/neo/neo-timeline').then((m) => ({ default: m.NeoTimeline })),
  { ssr: false },
)
const NeoFiles = dynamic(
  () => import('@/components/matrix/neo/neo-files').then((m) => ({ default: m.NeoFiles })),
  { ssr: false },
)
const NeoSelfModel = dynamic(
  () =>
    import('@/components/matrix/neo/neo-self-model').then((m) => ({
      default: m.NeoSelfModelOverlay,
    })),
  { ssr: false },
)
const AgentWalletSheet = dynamic(
  () =>
    import('@/components/matrix/agent-wallet-sheet').then((m) => ({
      default: m.AgentWalletSheet,
    })),
  { ssr: false },
)

export function Dashboard() {
  const chat = useChat()
  useHandoffAsk(chat.send)
  const [searchOpen, setSearchOpen] = useState(false)
  const [timelineOpen, setTimelineOpen] = useState(false)
  const [filesOpen, setFilesOpen] = useState(false)
  const [selfModelOpen, setSelfModelOpen] = useState(false)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [walletOpen, setWalletOpen] = useState(false)

  // The shared surface-state model the shell reads. `useSurfaceFeed` hydrates it
  // from the conversation's durable surface record on a cold open (GET
  // /construct/state replayed through the shared reducer), then tails the live
  // stream on the EXISTING chat SSE transport scoped to the active run — so the
  // environment rehydrates as the user left it and home/activity surfaces survive
  // reload (R1.2/R17.1/R17.5). Each surface carries its stable
  // `construct://{conversationId}/{surfaceId}` address, assigned by the reducer on
  // add (R7.1/R7.5). The feed-backed `rehydrate` resolves cold links by address
  // for depth navigation (R4.4).
  const { workspace, setWorkspace, rehydrate } = useSurfaceFeed(
    chat.conversationId,
    chat.activeIntentId,
  )

  // Depth navigation: tap a Timeline step → descend into its linked Stream at
  // raw; ascend pops (R17.3/R17.4). A cold link is rehydrated by address through
  // the feed first (R4.4); if it cannot be opened, the descent reports
  // unavailable in plain language and leaves the focus stack unchanged (R4.5).
  const descent = useDescent(workspace, setWorkspace, { rehydrate })

  // The Computer is FIRST-CLASS (DOJO req 4): the stage is ALWAYS mounted —
  // "Neo's Computer" is the environment centerpiece from the first paint, not
  // something that appears only after a run produced tool work. Its Desktop
  // screen carries the disposable desktop's power button, so the user can turn
  // the computer on at any moment with zero agent involvement; the legacy
  // NeoStep work (terminal / browser / sources / agents / media) joins it as
  // screens whenever a run produces any (legacyOnly, so the typed Construct
  // surfaces the feed renders are never doubled).
  //
  // The desktop binds to a conversation; before the first message mints one,
  // the reserved 'desktop' key stands in — the daemon is single-tenant with
  // one computer (MaxActive=1), and every conversation's panel resolves THE
  // live session server-side regardless of which key booted it.
  const dojoConversation = chat.conversationId ?? 'desktop'

  // The Computer is always MOUNTED (its power controls belong to the user, not
  // to a run) but the stage should not stand open with nothing in it when the
  // app is first opened. This says whether the Computer currently holds
  // anything worth looking at; the shell opens the stage the moment it does.
  const task = chat.task
  const environmentActive =
    chat.phase === 'thinking' ||
    chat.phase === 'working' ||
    !!task?.thinking ||
    (task?.steps?.length ?? 0) > 0 ||
    (task?.searches?.length ?? 0) > 0 ||
    (task?.finance?.length ?? 0) > 0 ||
    (task?.media?.length ?? 0) > 0 ||
    (task?.artifacts?.length ?? 0) > 0 ||
    (task?.todos?.length ?? 0) > 0 ||
    !!task?.swarm ||
    // A desktop that is booting or live IS the work — the user turned it on.
    (!!task?.dojo && task.dojo.state !== 'destroyed')

  return (
    <>
      <Shell
        workspace={workspace}
        environmentActive={environmentActive}
        handlers={{ onRespond: chat.respondAsk }}
        onDescendStep={descent.descend}
        onAscend={descent.ascend}
        descentNotice={descent.notice}
        onDismissNotice={descent.clearNotice}
        environment={
          <PanelErrorBoundary label="Neo's work">
            <NeoComputer
              task={chat.task ?? EMPTY_TASK}
              phase={chat.phase}
              reduce={false}
              showMedia
              legacyOnly
              onRespond={chat.respondAsk}
              conversationId={dojoConversation}
              className="h-full w-full"
            />
          </PanelErrorBoundary>
        }
        narration={
          <PanelErrorBoundary label="Chat">
            <NeoSurface
              embedded
              phase={chat.phase}
              task={chat.task}
              messages={chat.messages}
              send={chat.send}
              pendingGate={chat.pendingGate}
              resuming={chat.resuming}
              connectionRetrying={chat.connectionRetrying}
              answerGate={chat.answerGate}
              respondAsk={chat.respondAsk}
              dismissTask={chat.dismissTask}
              conversations={chat.conversations}
              conversationId={chat.conversationId}
              onVoiceIntent={chat.attachVoiceIntent}
              onSelectConversation={chat.selectConversation}
              onArchiveConversation={chat.archiveConversation}
              onRenameConversation={chat.renameConversation}
              onDeleteConversation={chat.deleteConversation}
              onForkConversation={chat.forkConversation}
              onNewChat={() => chat.reset()}
              onOpenHistory={() => {
                chat.refreshConversations()
                setSearchOpen(true)
              }}
              onOpenTimeline={() => setTimelineOpen(true)}
              onOpenFiles={() => setFilesOpen(true)}
              onOpenSettings={() => setSettingsOpen(true)}
              onOpenWallet={() => setWalletOpen(true)}
            />
          </PanelErrorBoundary>
        }
      />

      <NeoChatSearch
        open={searchOpen}
        onOpenChange={setSearchOpen}
        conversations={chat.conversations}
        activeConversationId={chat.conversationId}
        onSelect={(id) => chat.selectConversation(id)}
        onNewChat={() => chat.reset()}
        onArchive={chat.archiveConversation}
        onRename={chat.renameConversation}
        onDelete={chat.deleteConversation}
      />
      <NeoTimeline open={timelineOpen} onClose={() => setTimelineOpen(false)} />
      <NeoFiles open={filesOpen} onClose={() => setFilesOpen(false)} />
      <NeoSelfModel open={selfModelOpen} onClose={() => setSelfModelOpen(false)} />
      <AgentWalletSheet
        open={walletOpen}
        onClose={() => setWalletOpen(false)}
        onAskNeo={(prompt) => {
          setWalletOpen(false)
          chat.send(prompt)
        }}
      />
      <SettingsSheet
        open={settingsOpen}
        onOpenChange={setSettingsOpen}
        conversationId={chat.conversationId}
        intentId={chat.activeIntentId}
        onOpenWallet={() => setWalletOpen(true)}
        onOpenTimeline={() => setTimelineOpen(true)}
        onOpenSelfModel={() => setSelfModelOpen(true)}
        onOpenConversation={(id) => chat.selectConversation(id)}
      />
      <Toaster />
    </>
  )
}
