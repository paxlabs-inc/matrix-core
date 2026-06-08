export async function runHandle(handle, operation, args, ctx) {
  const deadline = Date.now() + (ctx.deadlineMs || 5000);
  const timer = setTimeout(() => {
    throw new Error('deadline exceeded');
  }, Math.max(0, deadline - Date.now()));
  try {
    const result = await handle(operation, args, ctx);
    return { outcome: 'ok', result, units: '1' };
  } finally {
    clearTimeout(timer);
  }
}
