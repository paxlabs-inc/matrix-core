'use client'

/**
 * ChatHistorySheet — browse and reopen past conversations.
 *
 * The structured human↔agent record (every chat thread) is persisted
 * server-side by the daemon, keyed by conversation_id. This slide-over
 * lists those threads newest-first; selecting one loads its turns from
 * the DB into the chat surface. Starting a new chat never loses the old
 * ones — they live here.
 */
import { Plus, MessageSquare } from '@/lib/matrix-icons'
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from '@/components/ui/sheet'
import { Button } from '@astryxdesign/core/Button'
import { List, ListItem } from '@astryxdesign/core/List'
import { Skeleton } from '@/components/ui/skeleton'
import type { ConversationSummary } from '@/lib/api/conversations'

export function ChatHistorySheet({
  open,
  onOpenChange,
  conversations,
  activeConversationId,
  loading,
  onSelect,
  onNewChat,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  conversations: ConversationSummary[]
  activeConversationId: string | null
  loading?: boolean
  onSelect: (id: string) => void
  onNewChat: () => void
}) {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="left" className="w-80 gap-0 p-0 sm:max-w-sm">
        <SheetHeader className="gap-1">
          <SheetTitle>Chat history</SheetTitle>
          <SheetDescription>Your past conversations, newest first.</SheetDescription>
        </SheetHeader>

        <div className="px-4 pb-3">
          <Button
            label="New chat"
            width="100%"
            variant="secondary"
            icon={<Plus />}
            onClick={() => {
              onNewChat()
              onOpenChange(false)
            }}
          />
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-4">
          {loading ? (
            <div className="flex flex-col gap-2 px-2">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-14 w-full rounded-lg" />
              ))}
            </div>
          ) : conversations.length === 0 ? (
            <div className="text-muted-foreground flex flex-col items-center gap-2 px-4 py-10 text-center text-sm">
              <MessageSquare className="size-5 opacity-60" />
              <p>No conversations yet. Start chatting and your threads will appear here.</p>
            </div>
          ) : (
            <List density="balanced">
              {conversations.map((c) => {
                const active = c.conversation_id === activeConversationId
                return (
                  <ListItem
                    key={c.conversation_id}
                    label={c.title || 'Untitled chat'}
                    description={
                      <span className="flex min-w-0 flex-col">
                        {c.preview ? <span className="truncate">{c.preview}</span> : null}
                        <span className="text-muted-foreground/70 text-[10px]">
                          {relativeTime(c.updated)} · {c.turn_count} message
                          {c.turn_count === 1 ? '' : 's'}
                        </span>
                      </span>
                    }
                    startContent={<MessageSquare className="size-4" />}
                    isSelected={active}
                    onClick={() => {
                      onSelect(c.conversation_id)
                      onOpenChange(false)
                    }}
                  />
                )
              })}
            </List>
          )}
        </div>
      </SheetContent>
    </Sheet>
  )
}

/** Compact "2h ago" style relative timestamp. Falls back to a date. */
function relativeTime(iso: string): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ''
  const diff = Date.now() - t
  const sec = Math.round(diff / 1000)
  if (sec < 60) return 'just now'
  const min = Math.round(sec / 60)
  if (min < 60) return `${min}m ago`
  const hr = Math.round(min / 60)
  if (hr < 24) return `${hr}h ago`
  const day = Math.round(hr / 24)
  if (day < 7) return `${day}d ago`
  return new Date(t).toLocaleDateString()
}
