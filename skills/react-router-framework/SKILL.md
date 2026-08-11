---
name: react-router-framework
description: React Router v7 framework mode (Remix) playbook for chat/realtime/collaborative apps — loaders/actions, nested routes, streaming, optimistic UI, and why not Next. Use when the app class is chat/realtime.
origin: Centra AI
---

# React Router Framework Mode Playbook

Doctrine pick for **chat / realtime / collaborative** apps (see
`rules/stack-selection/frontend.md`). React Router v7 in framework mode is the successor to
Remix: the loader/action model is a straight line from URL to server data to component, and
from user action to mutation to revalidation — exactly the shape of a live, session-driven
app. Choose this over Next for realtime; Next's RSC/route-cache model is a tax here, not a
benefit.

Use when: building a chat client, a collaborative editor, a live dashboard, or any app that
is mutation-heavy and per-user rather than content-cache-heavy.

## Init

```bash
npx create-react-router@latest chat-app
cd chat-app
npm run dev
```

This scaffolds framework mode (Vite plugin, SSR on, file/route config). Confirm
`@react-router/dev` is wired in `vite.config.ts` and routes are declared in `app/routes.ts`.

## Idiomatic structure

```
app/
  root.tsx              # <html> shell, <Outlet/>, global error boundary
  routes.ts             # route table (or file-based routes)
  routes/
    _app.tsx            # authed layout: sidebar of conversations
    _app.c.$id.tsx      # a single conversation: loader + action
    login.tsx
  lib/
    db.server.ts        # server-only data access (never imported client-side)
    session.server.ts   # cookie session, auth
```

Keep server-only code in `*.server.ts` so it is tree-shaken out of the client bundle.

## Data flow: loaders and actions

```ts
// app/routes/_app.c.$id.tsx
export async function loader({ params, request }) {
  const user = await requireUser(request);
  const messages = await db.messages.forThread(params.id, user.id);
  return { messages };
}

export async function action({ params, request }) {
  const user = await requireUser(request);
  const form = await request.formData();
  await db.messages.send(params.id, user.id, String(form.get("body")));
  return { ok: true }; // route revalidates automatically
}

export default function Conversation() {
  const { messages } = useLoaderData<typeof loader>();
  const fetcher = useFetcher(); // send without navigating
  return (
    <>
      <MessageList items={messages} />
      <fetcher.Form method="post">
        <input name="body" />
      </fetcher.Form>
    </>
  );
}
```

- **Loader** = read on navigation. **Action** = write; the route revalidates after.
- Use `useFetcher()` for sends/reactions that must not navigate — it is the workhorse of a
  chat UI.
- Nested routes model the conversation shell + active thread naturally via `<Outlet/>`.

## Realtime and optimistic UI

- Framework mode gives you the request/mutation seam; layer live updates on top:
  Server-Sent Events or a websocket subscription that triggers `revalidate()` (or updates a
  client store) when the server pushes.
- Optimistic UI: read `fetcher.formData` to render the in-flight message before the server
  confirms, then let revalidation reconcile.
- Stream slow loaders with `Await`/`Suspense` so the shell paints immediately.

## Testing

- Unit/component: **Vitest** + Testing Library (Vite-native, fast).
- Loaders/actions are plain functions — test them directly with a constructed `Request`.
- E2E: **Playwright** for the full chat flow (login, send, receive, revalidate).

## Build and deploy

```bash
npm run build     # build/server + build/client
npm start         # react-router-serve build/server/index.js
```

Deploys as a Node server (SSR). On Railway the detected start is `npm start`; set
`PORT`/session secret via env and use private networking to reach your DB — see the
`railway-deploy` skill. Choose a runtime adapter only if you target an edge platform.

## Common pitfalls

- Reaching for Next by reflex — for realtime you inherit RSC/caching complexity you do not
  want. This is the doctrine's headline deviation to avoid.
- Importing `*.server.ts` into client components — leaks server code/secrets into the bundle.
- Fetching in `useEffect` instead of a loader — you lose SSR, revalidation, and error
  boundaries.
- Full-page navigation for every send — use `useFetcher`.
- Skipping optimistic UI — a chat that waits for the round-trip feels broken.
