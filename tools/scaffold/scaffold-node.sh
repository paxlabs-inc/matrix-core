#!/usr/bin/env bash
# scaffold-node.sh — production Node.js service (Fastify + TypeScript).
# Tooling: fastify · pino · tsx · tsup · vitest · eslint/prettier · Docker.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/scaffold/_common.sh
# Resolved through the script directory at runtime.
# shellcheck disable=SC1091
source "$HERE/_common.sh"

common_parse_args "node" "$@"
require_cmd node
common_init_target
step "Node.js service (Fastify) → $PROJECT_SLUG"

write_if_absent package.json <<EOF
{
  "name": "@${SCAFFOLD_VCS_ORG}/${PROJECT_SLUG}",
  "version": "0.1.0",
  "private": true,
  "type": "module",
  "license": "${SCAFFOLD_LICENSE}",
  "engines": { "node": ">=22" },
  "scripts": {
    "dev": "tsx watch src/server.ts",
    "build": "tsup",
    "start": "node dist/server.js",
    "typecheck": "tsc --noEmit",
    "test": "vitest run",
    "test:cov": "vitest run --coverage",
    "lint": "eslint . && prettier --check .",
    "format": "eslint . --fix && prettier --write ."
  },
  "dependencies": {
    "fastify": "^5.2.0",
    "@fastify/helmet": "^13.0.0",
    "@fastify/sensible": "^6.0.0",
    "pino": "^9.5.0",
    "zod": "^3.24.0"
  },
  "devDependencies": {
    "@types/node": "^22.10.0",
    "@vitest/coverage-v8": "^3.0.0",
    "eslint": "^9.17.0",
    "pino-pretty": "^13.0.0",
    "prettier": "^3.4.0",
    "tsup": "^8.3.0",
    "tsx": "^4.19.0",
    "typescript": "^5.7.0",
    "typescript-eslint": "^8.19.0",
    "vitest": "^3.0.0"
  }
}
EOF

write_if_absent tsconfig.json <<'EOF'
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "Bundler",
    "outDir": "dist",
    "rootDir": "src",
    "strict": true,
    "noUncheckedIndexedAccess": true,
    "verbatimModuleSyntax": true,
    "isolatedModules": true,
    "resolveJsonModule": true,
    "skipLibCheck": true,
    "sourceMap": true
  },
  "include": ["src"]
}
EOF

write_if_absent tsup.config.ts <<'EOF'
import { defineConfig } from "tsup";

export default defineConfig({
  entry: ["src/server.ts"],
  format: ["esm"],
  target: "node22",
  clean: true,
  sourcemap: true,
});
EOF

# --- app source -------------------------------------------------------------
write_if_absent src/config.ts <<'EOF'
import { z } from "zod";

const schema = z.object({
  NODE_ENV: z.enum(["development", "test", "production"]).default("development"),
  HOST: z.string().default("0.0.0.0"),
  PORT: z.coerce.number().int().positive().default(3000),
  LOG_LEVEL: z.enum(["fatal", "error", "warn", "info", "debug", "trace"]).default("info"),
});

export const config = schema.parse(process.env);
export type Config = typeof config;
EOF

write_if_absent src/app.ts <<'EOF'
import Fastify, { type FastifyInstance } from "fastify";
import helmet from "@fastify/helmet";
import sensible from "@fastify/sensible";
import { config } from "./config.js";
import { health } from "./routes/health.js";

export async function buildApp(): Promise<FastifyInstance> {
  const app = Fastify({
    logger: {
      level: config.LOG_LEVEL,
      transport:
        config.NODE_ENV === "development"
          ? { target: "pino-pretty" }
          : undefined,
    },
  });

  await app.register(helmet);
  await app.register(sensible);
  await app.register(health);

  return app;
}
EOF

write_if_absent src/server.ts <<'EOF'
import { buildApp } from "./app.js";
import { config } from "./config.js";

const app = await buildApp();

const shutdown = async (signal: string) => {
  app.log.info({ signal }, "shutting down");
  await app.close();
  process.exit(0);
};
process.on("SIGTERM", () => void shutdown("SIGTERM"));
process.on("SIGINT", () => void shutdown("SIGINT"));

try {
  await app.listen({ host: config.HOST, port: config.PORT });
} catch (err) {
  app.log.error(err);
  process.exit(1);
}
EOF

write_if_absent src/routes/health.ts <<'EOF'
import type { FastifyInstance } from "fastify";

export async function health(app: FastifyInstance): Promise<void> {
  app.get("/healthz", async () => ({ status: "ok" }));
  app.get("/readyz", async () => ({ status: "ready" }));
}
EOF

write_if_absent src/app.test.ts <<'EOF'
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import type { FastifyInstance } from "fastify";
import { buildApp } from "./app.js";

let app: FastifyInstance;
beforeAll(async () => { app = await buildApp(); });
afterAll(async () => { await app.close(); });

describe("health", () => {
  it("GET /healthz → 200", async () => {
    const res = await app.inject({ method: "GET", url: "/healthz" });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ status: "ok" });
  });
});
EOF

# --- eslint / prettier / vitest --------------------------------------------
write_if_absent eslint.config.js <<'EOF'
import js from "@eslint/js";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist/**", "coverage/**", "node_modules/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  { rules: { "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_" }] } },
);
EOF
write_if_absent .prettierrc.json <<'EOF'
{ "semi": true, "singleQuote": false, "trailingComma": "all", "printWidth": 100 }
EOF
write_if_absent vitest.config.ts <<'EOF'
import { defineConfig } from "vitest/config";
export default defineConfig({
  test: { coverage: { provider: "v8", include: ["src/**"] } },
});
EOF

# --- env + container --------------------------------------------------------
write_if_absent .env.example <<'EOF'
NODE_ENV=development
HOST=0.0.0.0
PORT=3000
LOG_LEVEL=info
EOF

write_if_absent Dockerfile <<'EOF'
# syntax=docker/dockerfile:1
FROM node:22-slim AS base
ENV PNPM_HOME=/pnpm PATH=/pnpm:$PATH
RUN corepack enable
WORKDIR /app

FROM base AS deps
COPY package.json pnpm-lock.yaml* ./
RUN --mount=type=cache,id=pnpm,target=/pnpm/store pnpm install --frozen-lockfile

FROM base AS build
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN pnpm build && pnpm prune --prod

FROM node:22-slim AS runtime
ENV NODE_ENV=production
WORKDIR /app
USER node
COPY --from=build --chown=node:node /app/node_modules ./node_modules
COPY --from=build --chown=node:node /app/dist ./dist
COPY --chown=node:node package.json ./
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=3s CMD node -e "fetch('http://127.0.0.1:'+(process.env.PORT||3000)+'/readyz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"
CMD ["node", "dist/server.js"]
EOF

gen_dockerignore
write_if_absent .nvmrc <<'EOF'
22
EOF

gen_gitignore_base
gitignore_add "node / build" "node_modules/
dist/
*.tsbuildinfo
.eslintcache"

gen_github_ci "$(cat <<'YAML'
name: ci
on:
  push: { branches: [main] }
  pull_request:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: pnpm/action-setup@v4
      - uses: actions/setup-node@v4
        with: { node-version: 22, cache: pnpm }
      - run: pnpm install --frozen-lockfile
      - run: pnpm lint
      - run: pnpm typecheck
      - run: pnpm test:cov
      - run: pnpm build
YAML
)"

gen_editorconfig
gen_license
gen_docs
gen_contributing
gen_readme "Node.js service (Fastify)" \
  "${SCAFFOLD_PM} install" "$(pm_run_field dev)" "$(pm_run_field build)" \
  "$(pm_run_field test)" "$(pm_run_field lint)"

if [[ "$SCAFFOLD_INSTALL" == "1" ]]; then info "installing ($SCAFFOLD_PM)"; pm_install; fi

finalize_git
common_done "Node.js/Fastify service · $SCAFFOLD_PM"
