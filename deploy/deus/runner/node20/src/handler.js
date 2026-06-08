export async function handle(operation, args, _ctx) {
  if (operation === 'echo') {
    return { echo: args?.message ?? '' };
  }
  throw new Error(`unknown operation: ${operation}`);
}
