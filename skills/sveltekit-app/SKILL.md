---
name: sveltekit-app
description: SvelteKit playbook — routing, load functions, form actions, runes-based state, adapters, and testing. A defensible deviation when bundle size and authoring ergonomics are the priority. Use when the team picks Svelte.
origin: Matrix
---

# SvelteKit App Playbook

A **defensible deviation** in the stack doctrine (see `rules/stack-selection/frontend.md`).
SvelteKit is a strong pick when small bundles and authoring ergonomics are the priority and
the team is fluent in Svelte: minimal runtime, minimal boilerplate, compiler-driven
reactivity. State the reason in the SDR (bundle budget, team fluency) rather than picking it
by taste.

Use when: content or app-shaped surfaces where you want SPA-grade interactivity at a
fraction of the JS, and the team knows Svelte.

## Init

```bash
npx sv create my-app        # modern Svelte CLI; pick SvelteKit, TS, Vitest, Playwright
cd my-app
npm run dev
```

## Idiomatic structure

```
src/
  routes/
    +layout.svelte          # shared shell
    +page.svelte            # UI for a route
    +page.ts                # universal load (runs server + client)
    +page.server.ts         # server-only load + form actions
    +server.ts              # API endpoints (GET/POST → JSON)
  lib/
    server/                 # server-only modules ($lib/server, never client-imported)
    components/
  app.html                  # document shell
```

Filenames are the API: `+page`, `+layout`, `+server`, with `.server.ts` for server-only.

## Load functions and form actions

```ts
// +page.server.ts
export async function load({ locals }) {
  return { items: await locals.db.items.list() };
}

export const actions = {
  create: async ({ request, locals }) => {
    const form = await request.formData();
    await locals.db.items.create(String(form.get("name")));
    return { success: true };
  },
};
```

```svelte
<!-- +page.svelte -->
<script>
  export let data;                 // from load()
  import { enhance } from "$app/forms";
</script>
{#each data.items as item}<li>{item.name}</li>{/each}
<form method="POST" action="?/create" use:enhance>
  <input name="name" />
</form>
```

`use:enhance` gives progressive-enhancement form submits (works without JS, upgrades with
it). Endpoints in `+server.ts` for JSON APIs.

## State: runes (Svelte 5)

```svelte
<script>
  let count = $state(0);
  let doubled = $derived(count * 2);
  $effect(() => console.log(count));
</script>
```

Use `$state`/`$derived`/`$effect` runes in new code. Stores (`$lib/stores`) remain fine for
cross-component shared state.

## Testing

- Unit/component: **Vitest** + `@testing-library/svelte`.
- E2E: **Playwright** (`npm run test:e2e`).
- Load functions are plain functions — test them directly.

## Build, adapters, deploy

```bash
npm run build
npm run preview
```

The **adapter** decides the output target — pick it deliberately:

- `@sveltejs/adapter-node` — long-running Node server (SSR); the Railway default.
- `@sveltejs/adapter-static` — prerender a fully static site.
- `adapter-auto` — detects supported platforms; fine locally, be explicit in production.

On Railway with `adapter-node` the detected start is `node build`; set `PORT`/secrets via
env — see the `railway-deploy` skill.

## Common pitfalls

- Importing `$lib/server/*` into client code — build error / secret leak.
- Fetching in `onMount` instead of a `load` function — loses SSR and typed data.
- Leaving `adapter-auto` in production when the target is known — pin the adapter.
- Mixing legacy `export let` reactivity with runes inconsistently — commit to runes in new
  components.
