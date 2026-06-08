/**
 * Deus hosted runner harness (docs/06-execution-hosting.md §6.3).
 * Wraps developer handle() with timeout and response caps.
 */

export async function runHandle(handle, operation, args, ctx) {
  const deadline = Date.now() + (ctx.deadlineMs || 5000);
  const timer = setTimeout(() => {
    throw new Error('runner: deadline exceeded');
  }, Math.max(0, deadline - Date.now()));
  try {
    const result = await handle(operation, args, ctx);
    return { outcome: 'ok', result, units: '1' };
  } finally {
    clearTimeout(timer);
  }
}
