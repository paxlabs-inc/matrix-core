/**
 * Models — surfaces the gateway-whitelisted model pinning that the
 * router injects into every Fly Machine env. The daemon doesn't
 * expose a `/models` route in v1; the picker reads the current
 * compiler+executor pin from `/settings` and renders the static
 * dashboard catalogue (lib/matrix-data.ts) as the "available" list.
 */
import { models as catalog, type Model } from '@/lib/matrix-data'
import type { SettingsResponse } from '@/lib/api/types'

/** Map a Fireworks-style model id ("accounts/fireworks/models/X") to
 *  the dashboard catalogue id, or fall through to the original. */
export function settingsToActiveModelId(s: SettingsResponse | null | undefined): string | null {
  if (!s?.executor_model && !s?.compiler_model) return null
  const m = (s.executor_model ?? s.compiler_model ?? '').toLowerCase()
  // Order matters: specific variants (pro/flash) before the broad family.
  if (m.includes('deepseek') && m.includes('flash')) return 'mdl_deepseek_flash'
  if (m.includes('deepseek')) return 'mdl_deepseek_pro'
  if (m.includes('gpt-oss-120b') || m.includes('gpt-oss')) return 'mdl_gpt_oss_120b'
  if (m.includes('kimi')) return 'mdl_kimi_k26'
  if (m.includes('minimax')) return 'mdl_minimax_m27'
  if (m.includes('glm')) return 'mdl_glm_51'
  if (m.includes('qwen')) return 'mdl_qwen36_plus'
  return null
}

export function getModelCatalog(): Model[] {
  return catalog
}

export interface FriendlyModel {
  /** Human display name, e.g. "GPT-OSS 120B". */
  name: string
  /** Provider / family label, display only. */
  provider: string
}

/** Ordered rules mapping a raw gateway model id (Fireworks-style path or
 *  short slug) to a friendly name. First match wins; order matters because
 *  the broad families (e.g. "deepseek") must come after their specific
 *  variants (e.g. "deepseek-v4-pro"). */
const MODEL_RULES: Array<{ match: RegExp; name: string; provider: string }> = [
  { match: /gpt-?oss-?120b/i, name: 'GPT-OSS 120B', provider: 'OpenAI · open weights' },
  { match: /gpt-?oss-?20b/i, name: 'GPT-OSS 20B', provider: 'OpenAI · open weights' },
  { match: /deepseek.*pro/i, name: 'DeepSeek V4 Pro', provider: 'DeepSeek' },
  { match: /deepseek/i, name: 'DeepSeek V4 Flash', provider: 'DeepSeek' },
  { match: /qwen.*coder/i, name: 'Qwen3 Coder 480B', provider: 'Alibaba' },
  { match: /qwen/i, name: 'Qwen3', provider: 'Alibaba' },
  { match: /kimi/i, name: 'Kimi K2', provider: 'Moonshot' },
  { match: /glm/i, name: 'GLM 5.1', provider: 'Zhipu' },
  { match: /llama/i, name: 'Llama', provider: 'Meta' },
  { match: /gemini.*flash/i, name: 'Gemini Flash', provider: 'Google' },
  { match: /gemini/i, name: 'Gemini', provider: 'Google' },
  { match: /claude.*opus/i, name: 'Claude Opus', provider: 'Anthropic' },
  { match: /claude/i, name: 'Claude', provider: 'Anthropic' },
  { match: /gpt-?5/i, name: 'GPT-5', provider: 'OpenAI' },
  { match: /embed|bge|e5|nomic/i, name: 'Embeddings', provider: 'Vector index' },
]

/** Map a raw model id reported by the daemon to a friendly name + provider.
 *  Falls back to the last path segment so an unknown pin still renders
 *  something sensible instead of a Fireworks URL. */
export function friendlyModel(rawId: string | null | undefined): FriendlyModel | null {
  if (!rawId) return null
  const id = String(rawId).trim()
  if (!id) return null
  for (const rule of MODEL_RULES) {
    if (rule.match.test(id)) return { name: rule.name, provider: rule.provider }
  }
  const tail = id.split('/').pop() || id
  return { name: tail, provider: 'Custom runtime' }
}
