import assert from 'node:assert/strict'
import test from 'node:test'
import { loadConfig } from '../src/config.mjs'

const base = {
  SANDBOXD_TOKEN: 'internal-token',
  SANDBOXD_PREVIEW_DOMAIN: 'preview.example.com',
  RAILWAY_ENVIRONMENT_ID: 'env-test',
}

function configured(values) {
  const original = { ...process.env }
  Object.assign(process.env, base, values)
  delete process.env.RAILWAY_TOKEN
  delete process.env.RAILWAY_API_TOKEN
  Object.assign(process.env, values)
  try {
    return loadConfig()
  } finally {
    process.env = original
  }
}

test('project tokens use project-token authentication', () => {
  const config = configured({ RAILWAY_TOKEN: 'project-secret' })
  assert.equal(config.railwayToken, 'project-secret')
  assert.equal(config.railwayAuthType, 'project-token')
  assert.equal(config.railwayAuthVariable, 'RAILWAY_TOKEN')
})

test('account and workspace tokens use bearer authentication', () => {
  const config = configured({ RAILWAY_API_TOKEN: 'api-secret' })
  assert.equal(config.railwayToken, 'api-secret')
  assert.equal(config.railwayAuthType, 'bearer')
  assert.equal(config.railwayAuthVariable, 'RAILWAY_API_TOKEN')
})

test('ambiguous and missing Railway credentials fail fast', () => {
  assert.throws(() => configured({}), /RAILWAY_TOKEN or RAILWAY_API_TOKEN is required/)
  assert.throws(
    () => configured({ RAILWAY_TOKEN: 'project-secret', RAILWAY_API_TOKEN: 'api-secret' }),
    /set exactly one/,
  )
})
