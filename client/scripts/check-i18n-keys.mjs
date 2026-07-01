#!/usr/bin/env node
/**
 * Verify newly-added i18n namespaces exist in every locale.
 * Full parity is not required — non-English locales deep-merge over en.json
 * at runtime (see i18n/request.ts).
 */
import fs from 'node:fs'
import path from 'node:path'

const messagesDir = path.join(process.cwd(), 'messages')
const base = JSON.parse(fs.readFileSync(path.join(messagesDir, 'en.json'), 'utf8'))

/** Namespaces that must be present in every locale file (legal / safety copy). */
const REQUIRED_NAMESPACES = ['compliance', 'network', 'walletConfirm']

function collectKeys(obj, prefix = '') {
  const keys = []
  for (const [k, v] of Object.entries(obj)) {
    const p = prefix ? `${prefix}.${k}` : k
    if (v && typeof v === 'object' && !Array.isArray(v)) keys.push(...collectKeys(v, p))
    else keys.push(p)
  }
  return keys
}

const requiredKeys = REQUIRED_NAMESPACES.flatMap((ns) => {
  const section = base[ns]
  if (!section) return []
  return collectKeys(section, ns)
})

let failed = false

for (const file of fs
  .readdirSync(messagesDir)
  .filter((f) => f.endsWith('.json') && f !== 'en.json')) {
  const locale = JSON.parse(fs.readFileSync(path.join(messagesDir, file), 'utf8'))
  const keys = new Set(collectKeys(locale))
  for (const k of requiredKeys) {
    if (!keys.has(k)) {
      console.error(`✗ ${file}: missing required key "${k}"`)
      failed = true
    }
  }
}

if (failed) process.exit(1)
console.log(
  `✓ i18n required keys OK (${requiredKeys.length} keys × ${REQUIRED_NAMESPACES.length} namespaces)`,
)
