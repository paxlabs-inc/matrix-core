'use client'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'
import { cn } from '@/lib/utils'
import {
  IconAdjustmentsHorizontal,
  IconArrowUp,
  IconBrandFigma,
  IconCamera,
  IconCirclePlus,
  IconClipboard,
  IconFileUpload,
  IconHistory,
  IconLayoutDashboard,
  IconLink,
  IconPaperclip,
  IconPlayerPlay,
  IconPlus,
  IconSparkles,
  IconTemplate,
  IconX,
} from '@tabler/icons-react'
import Image from 'next/image'
import { useRef, useState } from 'react'

interface AttachedFile {
  id: string
  name: string
  file: File
  preview?: string
}

const ACTIONS = [
  { id: 'clone-screenshot', icon: IconCamera, label: 'Clone a Screenshot' },
  { id: 'import-figma', icon: IconBrandFigma, label: 'Import from Figma' },
  { id: 'upload-project', icon: IconFileUpload, label: 'Upload a Project' },
  { id: 'landing-page', icon: IconLayoutDashboard, label: 'Landing Page' },
]

export default function Ai04({ onSubmit }: { onSubmit?: (prompt: string) => void }) {
  const [prompt, setPrompt] = useState('')
  const [isDragOver, setIsDragOver] = useState(false)
  const [attachedFiles, setAttachedFiles] = useState<AttachedFile[]>([])
  const fileInputRef = useRef<HTMLInputElement>(null)

  const [settings, setSettings] = useState({
    autoComplete: true,
    streaming: false,
    showHistory: false,
  })

  const generateFileId = () => Math.random().toString(36).substring(7)
  const processFiles = (files: File[]) => {
    for (const file of files) {
      const fileId = generateFileId()
      const attachedFile: AttachedFile = {
        id: fileId,
        name: file.name,
        file,
      }

      if (file.type.startsWith('image/')) {
        const reader = new FileReader()
        reader.onload = () => {
          setAttachedFiles((prev) =>
            prev.map((f) => (f.id === fileId ? { ...f, preview: reader.result as string } : f)),
          )
        }
        reader.readAsDataURL(file)
      }

      setAttachedFiles((prev) => [...prev, attachedFile])
    }
  }
  const submitPrompt = () => {
    if (prompt.trim() && onSubmit) {
      onSubmit(prompt.trim())
      setPrompt('')
    }
  }
  const updateSetting = (key: keyof typeof settings, value: boolean) => {
    setSettings((prev) => ({ ...prev, [key]: value }))
  }
  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    submitPrompt()
  }
  const handleDragOver = (e: React.DragEvent) => {
    e.preventDefault()
    setIsDragOver(true)
  }
  const handleDragLeave = (e: React.DragEvent) => {
    e.preventDefault()
    setIsDragOver(false)
  }
  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault()
    setIsDragOver(false)

    const files = Array.from(e.dataTransfer.files)
    if (files.length > 0) {
      processFiles(files)
    }
  }
  const handleTextareaChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    setPrompt(e.target.value)
  }
  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      submitPrompt()
    }
  }

  const handleFileSelect = (e: React.ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(e.target.files || [])
    processFiles(files)

    if (fileInputRef.current) {
      fileInputRef.current.value = ''
    }
  }

  const handleRemoveFile = (fileId: string) => {
    setAttachedFiles((prev) => prev.filter((file) => file.id !== fileId))
  }

  return (
    <div className="mx-auto flex w-full flex-col gap-4">
      <h1 className="font-heading text-foreground text-center text-[29px] font-semibold tracking-tighter text-balance text-pretty sm:text-[32px] md:text-[46px]">
        Prompt. Refine. Ship.
      </h1>
      <h2 className="text-muted-foreground -my-5 pb-4 text-center text-xl text-balance">
        Build real, working software just by describing it
      </h2>

      <div className="relative z-10 mx-auto flex w-full max-w-2xl flex-col content-center">
        <form
          className="focus-within:border-ring overflow-visible rounded-xl border p-2 transition-colors duration-200"
          onDragLeave={handleDragLeave}
          onDragOver={handleDragOver}
          onDrop={handleDrop}
          onSubmit={handleSubmit}
        >
          {attachedFiles.length > 0 && (
            <div className="relative mb-2 flex w-fit items-center gap-2 overflow-hidden">
              {attachedFiles.map((file) => (
                <Badge
                  variant="outline"
                  className="group hover:bg-accent relative h-6 max-w-30 cursor-pointer overflow-hidden px-0 text-[13px] transition-colors"
                  key={file.id}
                >
                  <span className="flex h-full items-center gap-1.5 overflow-hidden pl-1 font-normal">
                    <div className="relative flex h-4 min-w-4 items-center justify-center">
                      {file.preview ? (
                        <Image
                          alt={file.name}
                          className="absolute inset-0 h-4 w-4 rounded border object-cover"
                          src={file.preview}
                          width={16}
                          height={16}
                        />
                      ) : (
                        <IconPaperclip className="opacity-60" size={12} />
                      )}
                    </div>
                    <span className="inline truncate overflow-hidden pr-1.5">{file.name}</span>
                  </span>
                  <button
                    className="text-muted-foreground focus-visible:bg-accent focus-visible:ring-ring focus-visible:ring-offset-background absolute right-1 z-10 rounded-sm p-0.5 opacity-0 group-hover:opacity-100 focus-visible:opacity-100 focus-visible:ring-2"
                    onClick={() => handleRemoveFile(file.id)}
                    type="button"
                  >
                    <IconX size={12} />
                  </button>
                </Badge>
              ))}
            </div>
          )}
          <Textarea
            className="max-h-50 min-h-12 resize-none rounded-none border-none bg-transparent! p-0 text-sm shadow-none focus-visible:border-transparent focus-visible:ring-0"
            onChange={handleTextareaChange}
            onKeyDown={handleKeyDown}
            placeholder="Ask anything"
            value={prompt}
          />

          <div className="flex items-center gap-1">
            <div className="flex items-end gap-0.5 sm:gap-1">
              <input
                className="sr-only"
                multiple
                onChange={handleFileSelect}
                ref={fileInputRef}
                type="file"
              />

              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button
                    className="ml-[-2px] rounded-md"
                    size="icon-sm"
                    type="button"
                    variant="ghost"
                    aria-label="Add attachments"
                  >
                    <IconPlus size={16} />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="start" className="max-w-xs rounded-2xl p-1.5">
                  <DropdownMenuGroup className="space-y-1">
                    <DropdownMenuItem
                      className="rounded-md text-xs"
                      onClick={() => fileInputRef.current?.click()}
                    >
                      <div className="flex items-center gap-2">
                        <IconPaperclip className="text-muted-foreground" size={16} />
                        <span>Attach Files</span>
                      </div>
                    </DropdownMenuItem>
                    <DropdownMenuItem className="rounded-md text-xs">
                      <div className="flex items-center gap-2">
                        <IconLink className="text-muted-foreground" size={16} />
                        <span>Import from URL</span>
                      </div>
                    </DropdownMenuItem>
                    <DropdownMenuItem className="rounded-md text-xs">
                      <div className="flex items-center gap-2">
                        <IconClipboard className="text-muted-foreground" size={16} />
                        <span>Paste from Clipboard</span>
                      </div>
                    </DropdownMenuItem>
                    <DropdownMenuItem className="rounded-md text-xs">
                      <div className="flex items-center gap-2">
                        <IconTemplate className="text-muted-foreground" size={16} />
                        <span>Use Template</span>
                      </div>
                    </DropdownMenuItem>
                  </DropdownMenuGroup>
                </DropdownMenuContent>
              </DropdownMenu>

              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button
                    className="rounded-md"
                    size="icon-sm"
                    type="button"
                    variant="ghost"
                    aria-label="Adjust settings"
                  >
                    <IconAdjustmentsHorizontal size={16} />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="start" className="w-48 rounded-2xl p-3">
                  <DropdownMenuGroup className="space-y-3">
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <IconSparkles className="text-muted-foreground" size={16} />
                        <Label className="text-xs">Auto-complete</Label>
                      </div>
                      <Switch
                        checked={settings.autoComplete}
                        className="scale-75"
                        onCheckedChange={(value) => updateSetting('autoComplete', value)}
                      />
                    </div>

                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <IconPlayerPlay className="text-muted-foreground" size={16} />
                        <Label className="text-xs">Streaming</Label>
                      </div>
                      <Switch
                        checked={settings.streaming}
                        className="scale-75"
                        onCheckedChange={(value) => updateSetting('streaming', value)}
                      />
                    </div>

                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <IconHistory className="text-muted-foreground" size={16} />
                        <Label className="text-xs">Show History</Label>
                      </div>
                      <Switch
                        checked={settings.showHistory}
                        className="scale-75"
                        onCheckedChange={(value) => updateSetting('showHistory', value)}
                      />
                    </div>
                  </DropdownMenuGroup>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>

            <div className="ml-auto flex items-center gap-0.5 sm:gap-1">
              <Button
                className="rounded-md"
                disabled={!prompt.trim()}
                size="icon-sm"
                type="submit"
                variant="default"
                aria-label="Send message"
              >
                <IconArrowUp size={16} />
              </Button>
            </div>
          </div>

          <div
            className={cn(
              'border-border bg-muted text-foreground pointer-events-none absolute inset-0 z-20 flex items-center justify-center rounded-[inherit] border border-dashed text-sm transition-opacity duration-200',
              isDragOver ? 'opacity-100' : 'opacity-0',
            )}
          >
            <span className="flex w-full items-center justify-center gap-1 font-medium">
              <IconCirclePlus className="min-w-4" size={16} />
              Drop files here to add as attachments
            </span>
          </div>
        </form>
      </div>

      <div className="mx-auto flex min-h-0 max-w-250 shrink-0 flex-wrap items-center justify-center gap-3">
        {ACTIONS.map((action) => (
          <Button className="gap-2 rounded-full" key={action.id} size="sm" variant="outline">
            <action.icon size={16} />
            {action.label}
          </Button>
        ))}
      </div>
    </div>
  )
}
