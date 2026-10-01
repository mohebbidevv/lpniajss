# `/projects` + dashboard plan

Now that ownership exists, this can finally be built scoped correctly from day one — which was the whole reason it got sequenced after auth.

## New repo method
`ProjectRepository.ListByUser(ctx, userID) ([]*Project, error)` — `ListByStatus` filters by status, not owner; nothing today lists "everything one user owns." New Postgres impl, `WHERE user_id = $1 ORDER BY created_at DESC`.

## Two endpoints

**`GET /projects`** — list. For each project, enough for a card with zero further round-trips:
```
id, name, slug, status, source_type, live_url, created_at, updated_at
```
`live_url` computed server-side as `http://{slug}.{domain}` — the use case takes just the domain string (not the whole `CaddyClient`, it never talks to Caddy), same as `dockerrun.ContainerAddress` only needed the domain, not the client.

**`GET /projects/{id}`** — detail. Same fields, plus current deployment status pulled via `DeploymentRepo.GetCurrentForProject` — already exists, already unused, same story as `SlugExists` and `GetCurrentForProject` before it. If the deployment failed/crashed, include the failure reason and exit code.

Both go through `mustOwnProject` — for the list, that's just `ListByUser` filtering at the query; for detail, `GetByID` + the same ownership check `Run`/`Stop`/`Rename` already use. A stranger's project ID 404s, doesn't 403 — consistent with what's already there.

## Response shape — don't serialize the entity directly
`entities.Project` carries fields that should never reach a client: `SourceLocation` is a server filesystem path, `UniqueKey` is an internal correlation ID, `Port` is legacy host-exec-only. Same reasoning as why `User.PasswordHash` got `json:"-"` instead of a real key — wrap in a dedicated `ProjectSummary`/`ProjectDetail` DTO (matching `UploadOutput`/`ImportGithubOutput` convention) rather than adding json tags to the entity and hoping nothing sensitive leaks.

## Two use cases
`ListProjectsUseCase` (`ProjectRepo.ListByUser` → map to DTOs), `GetProjectUseCase` (`GetByID` → `mustOwnProject` → `DeploymentRepo.GetCurrentForProject` → map to DTO). Both trivial, both testable with the same fake-repo pattern already in `slug_test.go`/`auth_test.go`.

## Wiring
Same shape as every other route: `InitializeListProjectsHandler`/`InitializeGetProjectHandler` in `server.go`, both wrapped in `cors(requireAuth(...))` like everything else already is.

## Worth bundling in vs. deferring again
- **`DELETE /projects/{id}`** — there's currently no way to ever remove a project. For something actually launching, I'd bundle this in now rather than defer a third time — stop the live deployment if any, remove the route, delete the row (or soft-delete, your call).
- **Deployment history** (a "past deploys" tab) — still deliberately deferred, needs `DeploymentRepository.ListByProject` which doesn't exist. Clean follow-up once the single-current-deployment view is proven, not a blocker for launch.

---

# What else, for production MVP

Short and ranked by how much it actually blocks launch:

1. **The Postgres credential mismatch is still unresolved** — nothing above can be tested end-to-end until this is fixed. Blocks everything, including finally applying the users migration.
2. **`secure_cookies: false` needs to flip to `true` behind real HTTPS** — auth silently doesn't work (cookie never sent) until this and TLS termination are both in place. Caddy can do automatic HTTPS itself; needs a real domain pointed at the box.
3. **Wildcard DNS + wildcard TLS cert** for `*.yourdomain.com` — every deployed app's subdomain depends on this, and wildcard certs need a DNS-01 challenge (not the default HTTP-01), which is a deliberate Caddy config step, not automatic.
4. **Login/register rate limiting** — flagged as a should-have back when auth was planned, still not built. A public register/login endpoint with no throttling is a standing invitation.
5. **Image GC (Phase 7, never started)** — every deploy tags a new image forever with nothing removing old ones. This is a slow-motion disk-fill in production, not a maybe.
6. **`DELETE /projects/{id}`** — see above, genuinely needed for a real product, not just dashboard polish.
7. **A `/healthz` endpoint** — whatever's going to keep this process alive in production (systemd, a process manager, an uptime check) needs something to poll. Nothing exists today.
8. **Structured logging, not `log.Printf` with profanity in it** — fine for a solo dev session, not fine once these logs go to a real log aggregator or another person reads them.

Everything else from this session (the worker-timeout fix, `image_ref` persistence, the label-ordering nit, retry-creates-duplicate-rows) is real but genuinely lower stakes than the eight above — those are correctness/cleanliness, these eight are "will actually break or leak in production."

Honestly — no, not completely. What I gave you is solid for **viewing and basic lifecycle** (list, detail, delete), but it's thin on the actual day-to-day **management** actions. Let me be specific about what I missed, because two of them are things I already flagged earlier in this exact conversation and then failed to carry forward.

## The real gap: env vars

This is the biggest miss, and it's not a new discovery — we identified this exact hole back when I first mapped out the frontend flow: env vars have **zero backend support**. No entity field, no table, no endpoint. `appEnv()` is hardcoded to `NODE_ENV=production` and nothing else, full stop. A dashboard where you can't set an API key or a database URL for your own deployed app isn't a "manage your project" dashboard — it's a read-only viewer with a stop button. This needs:
- An `env_vars` table (or a JSON column on `deployments`/`projects` — a table is cleaner if you ever want per-var metadata like "secret, don't echo back")
- `POST/PUT /projects/{id}/env` to set them
- Wiring into `appEnv()` in the deploy pipeline so they actually reach the container
- A real decision on secrets: do these get encrypted at rest, or is "only the owner can read their own row" sufficient for MVP? I'd lean toward the latter for now and flag encryption-at-rest as a fast-follow, not a blocker — but it's a decision, not a default.

## Second gap: there's no way to see what a *running* app is actually doing

Also flagged earlier and also not carried forward: `Runtime.Logs(handle, opts)` is fully implemented in both `dockerrun` and `hostexec` and completely unused by HTTP. Today, the only way to see any log output at all is to trigger a brand-new deploy via `/run` and watch the SSE stream of that build — there is no "just tail what's live right now." For actually maintaining a project — figuring out why it's throwing 500s at 2am — this matters more day-to-day than the detail page I planned. Needs a `GET /projects/{id}/logs` (SSE, same shape as the deploy stream, but sourced from `Runtime.Logs` against the *current* container instead of a fresh deploy).

## Third: I deferred rollback, and I should say plainly whether that's actually okay

I put "deployment history" in the deferred bucket because `image_ref` persistence only just landed and nothing's read it yet. But rollback specifically — "the last deploy broke prod, go back" — is core management, not a nice-to-have tab. For a real production MVP I'd reconsider deferring this: the machinery is mostly already there (images are immutable and tagged per deployment, `ImageBuilder.RemoveImage`/image GC aside, `Runtime.Start` just needs an existing `imageRef` instead of a fresh build). A minimal version — `POST /projects/{id}/rollback/{deploymentID}`: skip the build, start the old image, flip the route — is a much smaller lift than I made it sound, and I'd rather flag that honestly than have you find out it's missing after launch.

## Two smaller but real ones

- **No plain restart without a rebuild.** `/run/{id}` always rebuilds from source. If you just changed an env var (see gap one) or the app crashed for a transient reason, forcing a full rebuild every time is wasteful and slow. A `POST /projects/{id}/restart` that starts a new container from the *existing* current image, no build step, is cheap to add and covers the common case.
- **Git-sourced projects can't change which branch they track after import.** `RepoRef` is set once at import and nothing ever updates it. Minor, but a real "manage" gap specific to git projects.

## Revised priority for "actually a management dashboard"

1. List + detail (as planned)
2. **Env vars** — table, get/set endpoint, wire into the pipeline
3. **Live logs** — expose the already-built `Runtime.Logs`
4. **Rollback** — smaller than it sounds given what's already in place
5. Delete
6. Restart-without-rebuild
7. Deployment history as a full tab (list every past attempt, not just "roll back to X")

1 and 5 are what I originally gave you. 2–4 are what actually makes it a dashboard someone can run a real app from instead of just watch one.