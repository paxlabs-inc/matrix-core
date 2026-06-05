---
name: dev-commands
description: Development commands for monorepo projects. Use when running dev servers, building apps, linting, testing, or troubleshooting build issues. Triggers on yarn, dev, build, lint, test, desktop, mobile, web, extension, ios, android, compile, bundle.
allowed-tools: Bash, Read
---

# Development Commands

## Application Development Commands

**PLATFORM-SPECIFIC DEVELOPMENT** (adapt command names to your project):
- `yarn app:desktop` - Start desktop Electron app development
  - **Runtime**: 30-60 seconds to start
  - **Common issues**: Node version conflicts, missing native dependencies
  - **Troubleshooting**: Run `yarn clean && yarn reinstall` if startup fails

- `yarn app:web` - Start web development server
  - **Runtime**: 15-30 seconds to start
  - **Common issues**: Port already in use, webpack/bundler compilation errors
  - **Troubleshooting**: Kill existing processes on port, check console for specific errors

- `yarn app:ext` - Start browser extension development
  - **Runtime**: 20-40 seconds to start
  - **Common issues**: Manifest v3 validation errors, permission issues
  - **Troubleshooting**: Check extension manifest validity, verify content security policy

- `yarn app:ios` - Start iOS mobile development
  - **Runtime**: 1-2 minutes (includes Metro bundler)
  - **Common issues**: Xcode setup, simulator issues, pod install failures
  - **Prerequisites**: Xcode installed, iOS simulator available

- `yarn app:android` - Start Android mobile development
  - **Runtime**: 1-2 minutes (includes Metro bundler)
  - **Common issues**: Android SDK path, emulator setup, gradle build failures
  - **Prerequisites**: Android Studio, SDK tools, emulator configured

## Build Commands

**PRODUCTION BUILDS** (Use for final validation):
- `yarn app:desktop:build` - Build desktop app for all platforms
  - **Runtime**: 5-10 minutes (multi-platform build)
  - **Output**: Platform-specific installers in `apps/desktop/dist/`
  - **Common issues**: Code signing, platform-specific dependencies

- `yarn app:ext:build` - Build browser extension
  - **Runtime**: 2-3 minutes
  - **Output**: Extension packages in `apps/ext/dist/`
  - **Common issues**: Manifest validation, content security policy violations

- `yarn app:web:build` - Build web application
  - **Runtime**: 3-5 minutes
  - **Output**: Static files in `apps/web/dist/`
  - **Common issues**: Bundle size limits, missing environment variables

- `yarn app:native-bundle` - Bundle React Native app
  - **Runtime**: 3-5 minutes
  - **Common issues**: Native module linking, Metro bundler errors

## Development Tools & Quality Assurance

### Pre-commit Commands (Local Development)

**Use these for fast pre-commit validation:**
- `yarn lint:staged` - Lint only staged files (fast, recommended for pre-commit)
- `yarn tsc:staged` - Type check staged files

**Pre-commit workflow:**
```bash
yarn lint:staged && yarn tsc:staged && git commit -m "your message"
```

### CI Commands (Full Project Check)

**These run in CI pipeline, not for local pre-commit:**
- `yarn lint` - **CI ONLY** comprehensive linting
  - **Expected runtime**: 5-10 minutes
  - **Zero tolerance**: ALL warnings and errors MUST be fixed
- `yarn lint:only` - Quick lint check (all files)
- `yarn tsc:only` - Full TypeScript type check
- `yarn test` - Test execution

### Other Tools

- `yarn clean` - Clean all build artifacts and node_modules
- `yarn reinstall` - Full clean install (use when dependency issues occur)

## Testing

- Jest configuration in `jest.config.js`
- Test setup in `jest-setup.js`
- Tests located in `__tests__/` or `@tests/` directories within packages
