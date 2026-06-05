---
name: coding-patterns
description: Coding patterns and best practices for development. Use when writing React components, handling promises, error handling, or following code conventions. Triggers on react, component, hooks, promise, async, await, error, pattern, convention, typescript.
allowed-tools: Read, Grep, Glob, Write, Edit
---

# Coding Patterns and Best Practices

## Quick Reference

| Topic | Guide | Key Points |
|-------|-------|------------|
| Promise handling | [promise-handling.md](references/rules/promise-handling.md) | Always await or use `void`, never floating promises |
| React components | [react-components.md](references/rules/react-components.md) | Named imports, functional components, no FC type |
| Restricted patterns | [restricted-patterns.md](references/rules/restricted-patterns.md) | Forbidden: `toLocaleLowerCase`, direct hd-core import |

## Critical Rules Summary

### Promise Handling

```typescript
// ❌ FORBIDDEN - floating promise
apiCall();

// ✅ CORRECT
await apiCall();
// or
void apiCall(); // intentionally not awaited
```

### React Components

```typescript
// ❌ FORBIDDEN
import React, { FC } from 'react';
const MyComponent: FC<Props> = () => {};

// ✅ CORRECT
import { useState, useCallback } from 'react';
function MyComponent({ prop }: { prop: string }) {}
```

### Restricted Patterns

```typescript
// ❌ FORBIDDEN - Locale-dependent string methods
string.toLocaleLowerCase()

// ✅ CORRECT - Consistent behavior
string.toLowerCase()
```

## Related Skills

- `/date-formatting` - Date and time formatting
- `/i18n` - Internationalization and translations
- `/error-handling` - Error handling patterns
- `/cross-platform` - Platform-specific code
- `/code-quality` - Linting and code quality
- `/performance` - Performance optimization
- `/state-management` - State management patterns
- `/architecture` - Project structure and import rules
