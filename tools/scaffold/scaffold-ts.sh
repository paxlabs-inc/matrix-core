#!/usr/bin/env bash
# scaffold-ts.sh — production TypeScript library (dual ESM/CJS, publishable).
# Tooling: tsup · vitest · eslint (flat) · prettier · strict tsconfig.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_common.sh
source "$HERE/_common.sh"

common_parse_args "ts" "$@"
require_cmd node
common_init_target
step "TypeScript library → $PROJECT_SLUG"

PM_RUN_DEV="$(pm_run_field dev)"
PM_RUN_BUILD="$(pm_run_field build)"
PM_RUN_TEST="$(pm_run_field test)"
PM_RUN_LINT="$(pm_run_field lint)"

# --- package.json -----------------------------------------------------------
write_if_absent package.json <<EOF
{
  "name": "@${SCAFFOLD_VCS_ORG}/${PROJECT_SLUG}",
  "version": "0.1.0",
  "description": "",
  "type": "module",
  "license": "${SCAFFOLD_LICENSE}",
  "author": "${SCAFFOLD_AUTHOR} <${SCAFFOLD_EMAIL}>",
  "sideEffects": false,
  "files": ["dist"],
  "main": "./dist/index.cjs",
  "module": "./dist/index.js",
  "types": "./dist/index.d.ts",
  "exports": {
    ".": {
      "types": "./dist/index.d.ts",
      "import": "./dist/index.js",
      "require": "./dist/index.cjs"
    }
  },
  "engines": { "node": ">=22" },
  "scripts": {
    "dev": "tsup --watch",
    "build": "tsup",
    "typecheck": "tsc --noEmit",
    "test": "vitest run",
    "test:watch": "vitest",
    "test:cov": "vitest run --coverage",
    "lint": "eslint . && prettier --check .",
    "format": "eslint . --fix && prettier --write .",
    "prepublishOnly": "npm run build"
  },
  "devDependencies": {
    "@types/node": "^22.10.0",
    "@vitest/coverage-v8": "^3.0.0",
    "eslint": "^9.17.0",
    "prettier": "^3.4.0",
    "tsup": "^8.3.0",
    "typescript": "^5.7.0",
    "typescript-eslint": "^8.19.0",
    "vitest": "^3.0.0"
  }
}
EOF

# --- tsconfig ---------------------------------------------------------------
write_if_absent tsconfig.json <<'EOF'
{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["ES2023"],
    "module": "ESNext",
    "moduleResolution": "Bundler",
    "declaration": true,
    "declarationMap": true,
    "sourceMap": true,
    "outDir": "dist",
    "rootDir": "src",
    "strict": true,
    "noUncheckedIndexedAccess": true,
    "exactOptionalPropertyTypes": true,
    "noImplicitOverride": true,
    "noFallthroughCasesInSwitch": true,
    "forceConsistentCasingInFileNames": true,
    "verbatimModuleSyntax": true,
    "isolatedModules": true,
    "skipLibCheck": true
  },
  "include": ["src"],
  "exclude": ["dist", "node_modules"]
}
EOF

write_if_absent tsup.config.ts <<'EOF'
import { defineConfig } from "tsup";

export default defineConfig({
  entry: ["src/index.ts"],
  format: ["esm", "cjs"],
  dts: true,
  sourcemap: true,
  clean: true,
  treeshake: true,
  target: "es2022",
});
EOF

# --- eslint (flat) + prettier ----------------------------------------------
write_if_absent eslint.config.js <<'EOF'
import js from "@eslint/js";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist/**", "coverage/**", "node_modules/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommendedTypeChecked,
  {
    languageOptions: {
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    rules: {
      "@typescript-eslint/consistent-type-imports": "error",
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_" }],
    },
  },
);
EOF

write_if_absent .prettierrc.json <<'EOF'
{
  "semi": true,
  "singleQuote": false,
  "trailingComma": "all",
  "printWidth": 100
}
EOF
write_if_absent .prettierignore <<'EOF'
dist
coverage
pnpm-lock.yaml
package-lock.json
EOF

# --- source + test ----------------------------------------------------------
write_if_absent src/index.ts <<EOF
/**
 * ${PROJECT_SLUG}
 * @packageDocumentation
 */

export interface GreetOptions {
  readonly loud?: boolean;
}

export function greet(name: string, opts: GreetOptions = {}): string {
  const msg = \`Hello, \${name}\`;
  return opts.loud ? \`\${msg.toUpperCase()}!\` : msg;
}
EOF

write_if_absent src/index.test.ts <<'EOF'
import { describe, expect, it } from "vitest";
import { greet } from "./index.js";

describe("greet", () => {
  it("greets", () => {
    expect(greet("world")).toBe("Hello, world");
  });
  it("shouts when loud", () => {
    expect(greet("world", { loud: true })).toBe("HELLO, WORLD!");
  });
});
EOF

write_if_absent vitest.config.ts <<'EOF'
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    coverage: { provider: "v8", reporter: ["text", "lcov"], include: ["src/**"] },
  },
});
EOF

# --- runtime pin + shared skeleton -----------------------------------------
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
gen_readme "TypeScript library" \
  "${SCAFFOLD_PM} install" "$PM_RUN_DEV" "$PM_RUN_BUILD" "$PM_RUN_TEST" "$PM_RUN_LINT"

if [[ "$SCAFFOLD_INSTALL" == "1" ]]; then
  info "installing dependencies ($SCAFFOLD_PM)"; pm_install
fi

finalize_git
common_done "TypeScript library · $SCAFFOLD_PM"
