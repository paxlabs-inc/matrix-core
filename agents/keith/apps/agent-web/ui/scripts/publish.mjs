import { cp, mkdir, rm } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('..', import.meta.url))
const output = fileURLToPath(new URL('../out', import.meta.url))
const target = fileURLToPath(new URL('../../static/ui', import.meta.url))

if (!target.startsWith(fileURLToPath(new URL('../../static/', import.meta.url)))) {
  throw new Error('Refusing to publish outside the agent-web static directory')
}

await rm(target, { recursive: true, force: true })
await mkdir(target, { recursive: true })
await cp(output, target, { recursive: true })

console.log(`Published Keith Next.js assets from ${output.slice(root.length)} to ${target}`)
