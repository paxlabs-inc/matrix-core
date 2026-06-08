/** Example hosted handler — replace in uploaded artifacts. */
export async function handle(operation, args, ctx) {
  if (operation === 'echo') {
    return { echo: args?.message ?? '' };
  }
  throw new Error(`unknown operation: ${operation}`);
}
