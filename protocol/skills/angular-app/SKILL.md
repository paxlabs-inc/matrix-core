---
name: angular-app
description: Angular playbook for internal enterprise and forms-heavy admin apps — standalone components, typed reactive forms, signals, DI services, routing, and testing. Use when the app class is internal enterprise/admin.
origin: Centra AI
---

# Angular App Playbook

Doctrine pick for **internal enterprise / forms-heavy admin** UIs (see
`rules/stack-selection/frontend.md`). Angular's opinionatedness is the point: one router,
one forms system, DI, HttpClient, and i18n in a single frame that survives team turnover.

Use when: building a dense CRUD/admin surface, a forms-heavy internal tool, or any app where
long-term maintainability across a rotating team beats bespoke flexibility.

## Init

```bash
# Angular CLI is not on a fresh box; install on demand (see toolchain-bootstrap)
npm i -g @angular/cli
ng new admin-console --style=scss --ssr=false --routing
cd admin-console
ng serve
```

Prefer standalone components (default in modern Angular). Do not scaffold NgModules unless
integrating legacy code. Add `--ssr` only if you actually need server rendering — an
internal admin app rarely does.

## Idiomatic structure

```
src/app/
  core/            # singletons: auth, http interceptors, guards, app-wide services
  shared/          # dumb reusable components, pipes, directives
  features/
    users/
      users.routes.ts
      user-list.component.ts
      user-detail.component.ts
      users.service.ts
  app.config.ts    # providers: router, http, interceptors
  app.routes.ts    # top-level routes, lazy-load feature routes
```

- One feature = one folder with its own `*.routes.ts`, lazy-loaded from the top level.
- `core/` holds `providedIn: 'root'` services and interceptors. `shared/` holds presentational pieces with no business logic.

## Routing and lazy loading

```ts
// app.routes.ts
export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'users' },
  {
    path: 'users',
    canActivate: [authGuard],
    loadChildren: () => import('./features/users/users.routes').then(m => m.USERS_ROUTES),
  },
];
```

Lazy-load every feature with `loadChildren`/`loadComponent`. Use functional guards
(`CanActivateFn`) and functional interceptors (`HttpInterceptorFn`) — the class-based forms
are legacy.

## Data: signals + HttpClient

```ts
@Injectable({ providedIn: 'root' })
export class UsersService {
  private http = inject(HttpClient);
  list() { return this.http.get<User[]>('/api/users'); }
  create(u: NewUser) { return this.http.post<User>('/api/users', u); }
}
```

Use `inject()` over constructor injection in new code. Prefer **signals** for component
state and `toSignal()` to bridge Observables; keep RxJS for streams and HTTP. Turn on
`provideHttpClient(withInterceptors([authInterceptor]))` in `app.config.ts`.

## Typed reactive forms — the reason to be here

```ts
form = new FormGroup({
  email: new FormControl('', { nonNullable: true, validators: [Validators.required, Validators.email] }),
  role: new FormControl<Role>('viewer', { nonNullable: true }),
});
// form.value is fully typed; form.controls.email.errors is typed
```

Reactive forms, not template-driven, for anything non-trivial. `nonNullable: true` to avoid
`null` in the model. Build cross-field and async validators as functions. This typed-forms
ergonomics is why enterprise admin picks Angular — lean into it.

## Testing

- Unit: Jasmine + Karma ships with the CLI (`ng test`). Many teams swap to **Jest** or
  **Vitest** for speed — acceptable, state it.
- Component: `TestBed` with `HttpClientTestingModule` (or `provideHttpClientTesting()`) to
  assert requests without a network.
- E2E: Playwright (`ng e2e` scaffolds it). Protractor is dead — never use it.

## Build and deploy

```bash
ng build            # outputs to dist/<app>/browser, hashed and minified
```

Serve the static `browser/` output from any CDN/host; wire SPA fallback to `index.html`.
For Railway, the detected start is a static serve of `dist/` — see the `railway-deploy`
skill. Set `NG_APP_*`/`environment.ts` values at build time; do not ship secrets to the
client.

## Common pitfalls

- Scaffolding NgModules in a new app — use standalone components.
- Template-driven forms for complex validation — use typed reactive forms.
- Constructor injection everywhere — prefer `inject()`.
- Forgetting to lazy-load features — the initial bundle balloons.
- Reaching for Angular for a marketing site or a realtime chat app — wrong class; see the
  doctrine table.
- Leaving `zone.js` assumptions in code you want to move to zoneless/signals later.
