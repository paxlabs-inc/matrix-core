// runHandle invokes the developer handler under a hard deadline and wraps a
// successful return in the HostedInvokeResponse envelope. It throws on handler
// error or deadline; dispatch.js converts thrown errors into an error envelope.
export async function runHandle(handle, operation, args, ctx) {
  const deadlineMs = Number(ctx.deadlineMs) > 0 ? Number(ctx.deadlineMs) : 5000;
  const result = await withDeadline(handle(operation, args, ctx), deadlineMs);
  return { outcome: 'ok', result, units: '1' };
}

// withDeadline races a promise against a timer so a slow/hung handler cannot
// outlive its deadline. The timer is always cleared to avoid leaks.
function withDeadline(promise, ms) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error('deadline exceeded')), Math.max(0, ms));
  });
  return Promise.race([Promise.resolve(promise), timeout]).finally(() => clearTimeout(timer));
}
