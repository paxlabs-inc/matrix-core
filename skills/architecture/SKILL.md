---
name: architecture
description: Monorepo architecture and code organization. Use when understanding project structure, package relationships, import rules, or component organization. Triggers on architecture, structure, packages, imports, hierarchy, dependencies, monorepo, organization.
allowed-tools: Read, Grep, Glob
---

# Monorepo Architecture Overview

## Platform Structure (Example)
- **`apps/desktop/`** - Electron desktop app (Windows, macOS, Linux)
- **`apps/mobile/`** - React Native mobile app (iOS, Android)
- **`apps/ext/`** - Browser extension (Chrome, Firefox, Edge, Brave)
- **`apps/web/`** - Progressive web application
- **`apps/web-embed/`** - Embeddable components

## Core Packages (Example)
- **`packages/core/`** - Core business logic, cryptography, protocol implementations
- **`packages/kit/`** - Application logic, state management, API integrations
- **`packages/kit-bg/`** - Background services and workers
- **`packages/components/`** - Cross-platform UI components
- **`packages/shared/`** - Platform abstractions, utilities, build configurations

## Key Architectural Patterns
- **Cross-platform UI**: Universal components with platform-specific adaptations
- **Platform-specific files**: Use `.native.ts`, `.desktop.ts`, `.web.ts`, `.ext.ts` suffixes
- **State management**: Atomic state management (e.g., Jotai, Zustand)
- **Package isolation**: Clear boundaries between packages

## Code Organization

### File Naming Conventions
- Platform-specific implementations use suffixes: `.native.ts`, `.web.ts`, `.desktop.ts`, `.ext.ts`
- Component files use PascalCase: `ComponentName.tsx`
- Hook files use camelCase with `use` prefix: `useHookName.ts`
- Utility files use camelCase: `utilityName.ts`

### Import Patterns
- Use workspace references: `@{scope}/components`, `@{scope}/core`, `@{scope}/kit`
- Platform detection via shared utilities
- Conditional imports based on platform capabilities

### Import Hierarchy Rules - STRICTLY ENFORCED

**CRITICAL**: Violating these rules WILL break the build and cause circular dependencies.

**HIERARCHY (NEVER violate this order):**
- `@{scope}/shared` - **FORBIDDEN** to import from any other internal packages
- `@{scope}/components` - **ONLY** allowed to import from `shared`
- `@{scope}/kit-bg` - **ONLY** allowed to import from `shared` and `core` (NEVER `components` or `kit`)
- `@{scope}/kit` - Can import from `shared`, `components`, and `kit-bg`
- Apps (desktop/mobile/ext/web) - Can import from all packages

**BEFORE ADDING ANY IMPORT:**
1. Verify the import respects the hierarchy above
2. Check if the import creates a circular dependency
3. Run type check to validate no circular dependency introduced
4. If unsure, find an alternative approach that respects the hierarchy

**COMMON VIOLATIONS TO AVOID:**
- ❌ Importing from `kit` in `components`
- ❌ Importing from `components` in `kit-bg`
- ❌ Importing from `kit` in `core`
- ❌ Any "upward" imports in the hierarchy

### Component Structure
- UI components in `packages/components/src/`
- Business logic in `packages/kit/src/`
- Domain-specific code in `packages/core/src/`

## Deep Analysis & Architecture Consistency Framework

### Pre-Modification Analysis Protocol

**MANDATORY ANALYSIS STEPS** (Execute BEFORE any code changes):

1. **Scope Impact Assessment**
   - Identify ALL packages/apps affected by the change
   - Map dependencies that will be impacted (use `yarn why <package>` if needed)
   - Evaluate cross-platform implications (desktop/mobile/web/extension)
   - Assess backward compatibility requirements

2. **Pattern Consistency Verification**
   - Examine existing similar implementations in the codebase
   - Identify established patterns and conventions used
   - Verify new code follows identical patterns
   - Check naming conventions align with existing code

3. **Architecture Integrity Check**
   - Validate against monorepo import hierarchy rules
   - Ensure separation of concerns is maintained
   - Verify platform-specific code uses correct file extensions
   - Check that business logic stays in appropriate packages

4. **Performance Impact Evaluation**
   - Consider bundle size implications (especially for web/extension)
   - Evaluate runtime performance effects
   - Assess memory usage implications
   - Consider impact on application startup time

### Code Pattern Recognition Framework

**WHEN ADDING NEW FUNCTIONALITY:**
1. **Find Similar Examples**: Search codebase for similar implementations
2. **Extract Patterns**: Identify common approaches, naming, structure
3. **Follow Conventions**: Mirror existing patterns exactly
4. **Validate Consistency**: Ensure new code looks like existing code

**WHEN MODIFYING EXISTING CODE:**
1. **Understand Context**: Read surrounding code and imports
2. **Preserve Patterns**: Maintain existing architectural decisions
3. **Consistent Style**: Match existing code style and structure
4. **Validate Integration**: Ensure changes integrate seamlessly

### Architecture Validation Checklist

**BEFORE COMMITTING ANY CHANGES:**
- [ ] Import hierarchy rules respected (no upward imports)
- [ ] Platform-specific files use correct extensions
- [ ] Security patterns maintained
- [ ] Error handling follows established patterns
- [ ] State management patterns consistently applied
- [ ] UI component patterns followed
- [ ] Translation patterns properly implemented (if applicable)
- [ ] Testing patterns maintained and extended
