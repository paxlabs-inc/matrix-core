'use client'

/**
 * BugReportDialog — always-available bug/feedback reporting UI. Captures
 * the user's message plus structured context (app version, conversation/
 * trace id, user-agent, timestamp, optional log tail). Submits via
 * POST /reports. Confirms receipt without blocking; retries on failure
 * preserving the typed message. No jargon/stack traces in UX.
 */
import { useState } from 'react'
import { Bug, Loader2, Send } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import { toast } from 'sonner'
import { submitReport } from '@/lib/api/onboarding'
import { env } from '@/lib/env'

interface Props {
  children?: React.ReactNode
  conversationId?: string | null
  intentId?: string | null
}

export function BugReportDialog({ children, conversationId, intentId }: Props) {
  const t = useTranslations('onboarding')
  const [open, setOpen] = useState(false)
  const [message, setMessage] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const buildContext = () => {
    const ctx: Record<string, unknown> = {
      app_version: env.release,
      user_agent: typeof navigator !== 'undefined' ? navigator.userAgent : '',
      timestamp: new Date().toISOString(),
      locale: typeof navigator !== 'undefined' ? navigator.language : '',
    }
    if (conversationId) ctx.conversation_id = conversationId
    if (intentId) ctx.intent_id = intentId
    return ctx
  }

  const handleSubmit = async () => {
    if (!message.trim()) return
    setSubmitting(true)
    try {
      await submitReport({
        message: message.trim(),
        context: buildContext(),
      })
      toast.success(t('reportSent'))
      setMessage('')
      setOpen(false)
    } catch {
      toast.error(t('reportError'))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {children ?? (
          <Button variant="ghost" size="sm">
            <Bug className="size-4" />
            {t('reportBug')}
          </Button>
        )}
      </DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t('reportTitle')}</DialogTitle>
          <DialogDescription>{t('reportDesc')}</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <Textarea
            value={message}
            onChange={(e) => setMessage(e.target.value)}
            placeholder={t('reportPlaceholder')}
            rows={4}
            autoFocus
          />
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setOpen(false)}>
              {t('cancel')}
            </Button>
            <Button disabled={submitting || !message.trim()} onClick={() => void handleSubmit()}>
              {submitting ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <Send data-icon="inline-end" />
              )}
              {t('reportSubmit')}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
