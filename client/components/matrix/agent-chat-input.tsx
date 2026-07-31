'use client'

import type React from 'react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { Lightbulb, Mic, Globe, Paperclip, Send, X } from 'lucide-react'
import { AnimatePresence, motion, type Variants } from 'motion/react'
import { toast } from 'sonner'

export interface AIChatInputProps {
  onSubmit?: (text: string) => void
  disabled?: boolean
}

const PLACEHOLDERS = [
  'Check the latest PAX price and 24h move',
  'Audit this contract for risks before I ape in',
  'What can you help me do?',
  'Summarize my recent on-chain activity',
  'Find the top movers on Paxeer right now',
  'Stake 10 PAX and report the position',
]

// Grow the composer with its content up to ~6 lines, then scroll internally.
const MAX_TEXTAREA_HEIGHT = 160

// Minimal shape of the Web Speech API we rely on — it is not in lib.dom yet.
interface SpeechAlternative {
  transcript: string
}
interface SpeechResult {
  0: SpeechAlternative
}
interface SpeechResultList {
  length: number
  [index: number]: SpeechResult
}
interface SpeechRecognitionEventLike {
  results: SpeechResultList
}
interface SpeechRecognitionLike {
  lang: string
  continuous: boolean
  interimResults: boolean
  start: () => void
  stop: () => void
  onresult: ((event: SpeechRecognitionEventLike) => void) | null
  onerror: (() => void) | null
  onend: (() => void) | null
}
type SpeechRecognitionCtor = new () => SpeechRecognitionLike

const AIChatInput = ({ onSubmit, disabled = false }: AIChatInputProps = {}) => {
  const [placeholderIndex, setPlaceholderIndex] = useState(0)
  const [showPlaceholder, setShowPlaceholder] = useState(true)
  const [isActive, setIsActive] = useState(false)
  const [thinkActive, setThinkActive] = useState(false)
  const [deepSearchActive, setDeepSearchActive] = useState(false)
  const [inputValue, setInputValue] = useState('')
  const [attachments, setAttachments] = useState<File[]>([])
  const [listening, setListening] = useState(false)

  const wrapperRef = useRef<HTMLDivElement>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const recognitionRef = useRef<SpeechRecognitionLike | null>(null)
  const voiceBaseRef = useRef('')

  const expanded = isActive || inputValue.length > 0 || attachments.length > 0

  // Auto-grow the textarea so wrapped lines push the box DOWN instead of the
  // text scrolling sideways and hiding what was typed.
  const resizeTextarea = useCallback(() => {
    const el = textareaRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, MAX_TEXTAREA_HEIGHT)}px`
  }, [])

  useEffect(() => {
    resizeTextarea()
  }, [inputValue, resizeTextarea])

  const submit = () => {
    const value = inputValue.trim()
    if ((!value && attachments.length === 0) || disabled) return
    // No upload pipeline exists, so surface attachments as a note on the turn
    // rather than dropping them silently.
    const note = attachments.length
      ? `${value ? '\n\n' : ''}[Attached: ${attachments.map((f) => f.name).join(', ')}]`
      : ''
    onSubmit?.(`${value}${note}`)
    setInputValue('')
    setAttachments([])
  }

  // Cycle placeholder copy only while the composer is idle and empty.
  useEffect(() => {
    if (expanded) return

    const interval = setInterval(() => {
      setShowPlaceholder(false)
      setTimeout(() => {
        setPlaceholderIndex((prev) => (prev + 1) % PLACEHOLDERS.length)
        setShowPlaceholder(true)
      }, 400)
    }, 3000)

    return () => clearInterval(interval)
  }, [expanded])

  // Collapse the composer when clicking away from it while it is empty.
  useEffect(() => {
    const handleClickOutside = (event: MouseEvent) => {
      if (wrapperRef.current && !wrapperRef.current.contains(event.target as Node)) {
        if (!inputValue && attachments.length === 0) setIsActive(false)
      }
    }

    document.addEventListener('mousedown', handleClickOutside)
    return () => document.removeEventListener('mousedown', handleClickOutside)
  }, [inputValue, attachments.length])

  // Stop any in-flight dictation if the composer unmounts.
  useEffect(() => () => recognitionRef.current?.stop(), [])

  const handleActivate = () => setIsActive(true)

  // --- Attachments -------------------------------------------------------
  const openFilePicker = () => fileInputRef.current?.click()

  const handleFilesChosen = (event: React.ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? [])
    if (files.length) {
      setAttachments((prev) => [...prev, ...files])
      setIsActive(true)
      toast.success(
        files.length === 1 ? `Attached ${files[0].name}` : `Attached ${files.length} files`,
      )
    }
    // Reset so re-selecting the same file still fires onChange.
    event.target.value = ''
  }

  const removeAttachment = (index: number) =>
    setAttachments((prev) => prev.filter((_, i) => i !== index))

  // --- Voice dictation (Web Speech API) ---------------------------------
  const startVoice = () => {
    const speech = window as unknown as {
      SpeechRecognition?: SpeechRecognitionCtor
      webkitSpeechRecognition?: SpeechRecognitionCtor
    }
    const Recognition = speech.SpeechRecognition ?? speech.webkitSpeechRecognition
    if (!Recognition) {
      toast.error('Voice input isn’t supported in this browser.')
      return
    }

    const recognition = new Recognition()
    recognition.lang = navigator.language || 'en-US'
    recognition.interimResults = true
    recognition.continuous = false
    // Append dictation after whatever is already typed.
    voiceBaseRef.current = inputValue ? `${inputValue.trimEnd()} ` : ''

    recognition.onresult = (event) => {
      let transcript = ''
      for (let i = 0; i < event.results.length; i++) {
        transcript += event.results[i][0].transcript
      }
      setInputValue(voiceBaseRef.current + transcript)
    }
    recognition.onerror = () => setListening(false)
    recognition.onend = () => {
      setListening(false)
      recognitionRef.current = null
    }

    recognitionRef.current = recognition
    setIsActive(true)
    setListening(true)
    recognition.start()
  }

  const toggleVoice = () => {
    if (listening) recognitionRef.current?.stop()
    else startVoice()
  }

  const containerVariants: Variants = {
    collapsed: {
      boxShadow: '0 2px 8px 0 rgba(0,0,0,0.08)',
      transition: { type: 'spring', stiffness: 120, damping: 18 },
    },
    expanded: {
      boxShadow: '0 8px 32px 0 rgba(0,0,0,0.16)',
      transition: { type: 'spring', stiffness: 120, damping: 18 },
    },
  }

  const placeholderContainerVariants: Variants = {
    initial: {},
    animate: { transition: { staggerChildren: 0.025 } },
    exit: { transition: { staggerChildren: 0.015, staggerDirection: -1 } },
  }

  const letterVariants: Variants = {
    initial: {
      opacity: 0,
      filter: 'blur(12px)',
      y: 10,
    },
    animate: {
      opacity: 1,
      filter: 'blur(0px)',
      y: 0,
      transition: {
        opacity: { duration: 0.25 },
        filter: { duration: 0.4 },
        y: { type: 'spring', stiffness: 80, damping: 20 },
      },
    },
    exit: {
      opacity: 0,
      filter: 'blur(12px)',
      y: -10,
      transition: {
        opacity: { duration: 0.2 },
        filter: { duration: 0.3 },
        y: { type: 'spring', stiffness: 80, damping: 20 },
      },
    },
  }

  return (
    <div className="text-foreground w-full">
      <input
        ref={fileInputRef}
        type="file"
        multiple
        className="hidden"
        onChange={handleFilesChosen}
      />
      <motion.div
        ref={wrapperRef}
        className="border-border bg-card w-full border shadow-sm"
        variants={containerVariants}
        animate={expanded ? 'expanded' : 'collapsed'}
        initial="collapsed"
        style={{ overflow: 'hidden', borderRadius: 28 }}
        onClick={handleActivate}
      >
        <div className="flex w-full flex-col items-stretch">
          {/* Attachment chips */}
          {attachments.length > 0 && (
            <div className="flex flex-wrap gap-2 px-4 pt-3">
              {attachments.map((file, index) => (
                <span
                  key={`${file.name}-${index}`}
                  className="bg-muted text-muted-foreground flex items-center gap-1.5 rounded-full py-1 pr-1 pl-3 text-xs"
                >
                  <Paperclip size={12} className="shrink-0" />
                  <span className="max-w-[12rem] truncate">{file.name}</span>
                  <button
                    type="button"
                    title="Remove attachment"
                    className="hover:bg-background/60 hover:text-foreground rounded-full p-1 transition"
                    onClick={(e) => {
                      e.stopPropagation()
                      removeAttachment(index)
                    }}
                  >
                    <X size={12} />
                  </button>
                </span>
              ))}
            </div>
          )}

          {/* Input Row */}
          <div className="flex w-full items-end gap-2 p-3">
            <button
              className="hover:bg-muted text-muted-foreground hover:text-foreground rounded-full p-3 transition disabled:opacity-50"
              title="Attach file"
              type="button"
              disabled={disabled}
              onClick={(e) => {
                e.stopPropagation()
                openFilePicker()
              }}
            >
              <Paperclip size={20} />
            </button>

            {/* Text Input & Placeholder */}
            <div className="relative flex-1">
              <textarea
                ref={textareaRef}
                rows={1}
                value={inputValue}
                disabled={disabled}
                aria-label="Message"
                onChange={(e) => setInputValue(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey) {
                    e.preventDefault()
                    submit()
                  }
                }}
                className="text-foreground block max-h-40 w-full resize-none border-0 bg-transparent py-2 text-base font-normal outline-0 disabled:opacity-60"
                style={{ position: 'relative', zIndex: 1 }}
                onFocus={handleActivate}
              />
              <div className="pointer-events-none absolute top-0 left-0 flex h-full w-full items-center px-3 py-2">
                <AnimatePresence mode="wait">
                  {showPlaceholder && !expanded && (
                    <motion.span
                      key={placeholderIndex}
                      className="text-muted-foreground pointer-events-none absolute top-1/2 left-0 -translate-y-1/2 select-none"
                      style={{
                        whiteSpace: 'nowrap',
                        overflow: 'hidden',
                        textOverflow: 'ellipsis',
                        zIndex: 0,
                      }}
                      variants={placeholderContainerVariants}
                      initial="initial"
                      animate="animate"
                      exit="exit"
                    >
                      {PLACEHOLDERS[placeholderIndex].split('').map((char, i) => (
                        <motion.span
                          key={i}
                          variants={letterVariants}
                          style={{ display: 'inline-block' }}
                        >
                          {char === ' ' ? ' ' : char}
                        </motion.span>
                      ))}
                    </motion.span>
                  )}
                </AnimatePresence>
              </div>
            </div>

            <button
              className={`rounded-full p-3 transition disabled:opacity-50 ${
                listening
                  ? 'bg-red-500/15 text-red-500'
                  : 'hover:bg-muted text-muted-foreground hover:text-foreground'
              }`}
              title={listening ? 'Stop voice input' : 'Voice input'}
              type="button"
              aria-pressed={listening}
              disabled={disabled}
              onClick={(e) => {
                e.stopPropagation()
                toggleVoice()
              }}
            >
              <Mic size={20} />
            </button>
            <button
              className="bg-primary text-primary-foreground hover:bg-primary/90 flex items-center justify-center gap-1 rounded-full p-3 font-medium disabled:opacity-50"
              title="Send"
              type="button"
              disabled={disabled}
              onClick={(e) => {
                e.stopPropagation()
                submit()
              }}
            >
              <Send size={18} />
            </button>
          </div>

          {/* Expanded Controls */}
          <AnimatePresence initial={false}>
            {expanded && (
              <motion.div
                key="controls"
                initial={{ height: 0, opacity: 0 }}
                animate={{ height: 'auto', opacity: 1 }}
                exit={{ height: 0, opacity: 0 }}
                transition={{ duration: 0.25, ease: 'easeOut' }}
                style={{ overflow: 'hidden' }}
              >
                <div className="flex w-full items-center justify-start gap-3 px-4 pb-3 text-sm">
                  {/* Think Toggle */}
                  <button
                    className={`group flex items-center gap-1 rounded-full px-4 py-2 font-medium transition-all ${
                      thinkActive
                        ? 'bg-blue-600/10 text-blue-950 outline outline-blue-600/60'
                        : 'bg-muted text-muted-foreground hover:bg-muted/80'
                    }`}
                    title="Think"
                    type="button"
                    onClick={(e) => {
                      e.stopPropagation()
                      setThinkActive((a) => !a)
                    }}
                  >
                    <Lightbulb className="transition-all group-hover:fill-yellow-300" size={18} />
                    Think
                  </button>

                  {/* Deep Search Toggle */}
                  <motion.button
                    className={`flex items-center justify-start gap-1 overflow-hidden rounded-full px-4 py-2 font-medium whitespace-nowrap transition ${
                      deepSearchActive
                        ? 'bg-blue-600/10 text-blue-950 outline outline-blue-600/60'
                        : 'bg-muted text-muted-foreground hover:bg-muted/80'
                    }`}
                    title="Deep Search"
                    type="button"
                    onClick={(e) => {
                      e.stopPropagation()
                      setDeepSearchActive((a) => !a)
                    }}
                    initial={false}
                    animate={{
                      width: deepSearchActive ? 125 : 36,
                      paddingLeft: deepSearchActive ? 8 : 9,
                    }}
                  >
                    <div className="flex-1">
                      <Globe size={18} />
                    </div>
                    <motion.span
                      className="pb-[2px]"
                      initial={false}
                      animate={{
                        opacity: deepSearchActive ? 1 : 0,
                      }}
                    >
                      Deep Search
                    </motion.span>
                  </motion.button>
                </div>
              </motion.div>
            )}
          </AnimatePresence>
        </div>
      </motion.div>
    </div>
  )
}

export { AIChatInput }
