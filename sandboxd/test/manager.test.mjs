import assert from 'node:assert/strict'
import test from 'node:test'
import { dependencyInstallEnv } from '../src/manager.mjs'

test('dependency installation cannot inherit production-only package filtering', () => {
  assert.deepEqual(dependencyInstallEnv, {
    NODE_ENV: 'development',
    NPM_CONFIG_PRODUCTION: 'false',
    NPM_CONFIG_OMIT: '',
    YARN_PRODUCTION: 'false',
  })
})
