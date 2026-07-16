'use client'

import { createContext, use } from 'react'

export interface ToolUIContextValue {
  id: string
  surfaceMounted: boolean
  setSurfaceMounted: (mounted: boolean) => void
}

export const ToolUIContext = createContext<ToolUIContextValue | null>(null)

export function useOptionalToolUI(): ToolUIContextValue | null {
  return use(ToolUIContext)
}

export function useToolUI(): ToolUIContextValue {
  const context = useOptionalToolUI()

  if (!context) {
    throw new Error('ToolUI context is missing. Wrap LocalActions/DecisionActions with <ToolUI>.')
  }

  return context
}
