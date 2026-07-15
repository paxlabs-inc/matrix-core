function integer(name, fallback, min, max) {
  const raw = process.env[name]
  if (!raw) return fallback
  const value = Number.parseInt(raw, 10)
  if (!Number.isInteger(value) || value < min || value > max) {
    throw new Error(`${name} must be an integer from ${min} to ${max}`)
  }
  return value
}

function required(name) {
  const value = process.env[name]?.trim()
  if (!value) throw new Error(`${name} is required`)
  return value
}

export function loadConfig() {
  const domain = required('SANDBOXD_PREVIEW_DOMAIN').toLowerCase().replace(/^\*\./, '').replace(/\.$/, '')
  return {
    addr: process.env.SANDBOXD_ADDR || '0.0.0.0',
    port: integer('SANDBOXD_PORT', 8092, 1, 65535),
    token: required('SANDBOXD_TOKEN'),
    previewDomain: domain,
    publicScheme: process.env.SANDBOXD_PUBLIC_SCHEME || 'https',
    railwayToken: process.env.RAILWAY_TOKEN || process.env.RAILWAY_API_TOKEN || '',
    railwayAuthType: process.env.RAILWAY_TOKEN ? 'project-token' : 'bearer',
    environmentId: required('RAILWAY_ENVIRONMENT_ID'),
    maxPerUser: integer('SANDBOXD_MAX_PER_USER', 3, 1, 20),
    defaultTTLSeconds: integer('SANDBOXD_DEFAULT_TTL_SECONDS', 1800, 60, 86400),
    maxTTLSeconds: integer('SANDBOXD_MAX_TTL_SECONDS', 7200, 60, 86400),
    maxFiles: integer('SANDBOXD_MAX_FILES', 6000, 1, 20000),
    maxUploadBytes: integer('SANDBOXD_MAX_UPLOAD_BYTES', 50 * 1024 * 1024, 1024, 200 * 1024 * 1024),
    maxProxyBodyBytes: integer('SANDBOXD_MAX_PROXY_BODY_BYTES', 16 * 1024 * 1024, 1024, 100 * 1024 * 1024),
  }
}
