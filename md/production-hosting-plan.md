# Production hosting plan

Covers the items you named — live logs, per-user project cap, image
lifecycle, redeploy, storage/user limits, error handling — plus what I'd
add. No code yet.

## 1. Live logs — `GET /projects/{id}/logs` (SSE)

The hard part doesn't exist to build: `Runtime.Logs(handle, opts)` is
already implemented for both `dockerrun` and `hostexec`, just never called
from HTTP. This is almost entirely wiring, same shape as `/run/{id}`:

- `GetProjectLogsUseCase`: `GetByID` → `mustOwnProject` → if
  `CurrentDeploymentID == nil`, error ("nothing deployed yet") → look up
  the deployment's `ContainerID` → `Runtime.Logs(handle, opts)` → return the
  channel.
- Handler: identical SSE scaffolding to `run_handler.go` (`text/event-stream`,
  flusher, request-context cancellation on client disconnect) but sourced
  from this channel instead of `LogRegistry`.
- One real decision: `LogOptions` presumably supports `Follow` and `Tail` —
  default to `Follow: true, Tail: 200` or similar so a client gets recent
  context immediately instead of only new lines from the moment they
  connected.
- Route: `GET /projects/{id}/logs`, `requireAuth`, same ownership semantics
  as everything else (404 not 403).

## 2. Max 2 projects per user

Enforce it where ownership is actually established — there are two such
places now because of the anonymous-submit flow:

- `UploadProjectUseCase.Execute` / `ImportGithubProjectUseCase.Execute`,
  when `input.UserID != ""` (an already-logged-in caller creating directly).
- `ClaimProjectUseCase.Execute`, when a pending anonymous project is being
  attached to an account.

Both funnel through one helper, `enforceProjectLimit(ctx, repo, userID)`,
using `ListByUser` (already built for the dashboard) and a constant
`maxProjectsPerUser = 2`. Rejected with a clear, distinct message ("you've
reached the 2-project limit — delete a project to deploy a new one") so the
frontend can show it as-is rather than a generic error, and a 409 on the
wire.

**Open question for you:** does hitting the cap on an *anonymous* submission
(steps 1 of the claim flow, before signup) mean the upload succeeds but the
claim fails after they sign up — annoying but not dangerous — or should
`/upload` itself reject anonymous submissions once... from whom? Anonymous
requests have no identity yet to count against. I'd say: don't try to
enforce the cap pre-signup at all (you can't, there's no user yet), just
make the claim failure message clear. Confirm that's fine before I build it.

## 3. Image lifecycle — stop tagging one image per deploy forever

Two separate leaks in `deploy_pipeline.go` today, not one:

- **Successful redeploy**: `previous.ContainerID` gets stopped/removed in
  the teardown block at the end of `Deploy`, but `previous.ImageRef` (the
  *image*, recorded via `SetImageRef` from the previous deploy) is never
  passed to `Builder.RemoveImage`. Every successful redeploy leaves the
  prior image on disk permanently.
- **Failed deploy**: `Build()` succeeds and `imageRef` is recorded, but then
  `Start`/`WaitReady`/`RegisterRoute` fails → `p.fail(...)` runs and never
  removes the image that *was* built for this failed attempt either. Every
  failed deploy leaks an image too, arguably worse since it happens on every
  retry during debugging.

Fix: in the success-path teardown block, after `Runtime.Remove(prevHandle)`
succeeds, call `Builder.RemoveImage(ctx, previous.ImageRef)` (best-effort,
log-and-continue like the other teardown steps — a stuck image isn't worth
failing an otherwise-successful deploy over). In `fail()`, same best-effort
call for the image that was just built for *this* attempt, using the
`imageRef` already in scope in `Deploy` at each call site.

This alone stops the unbounded growth for the common case. It does **not**
replace a periodic GC pass — a crash mid-teardown, or a process restart
between `Build` succeeding and the removal call running, still leaves an
orphan. Phase 7 from the original docker plan (sweep, keep last N images per
project) is still worth doing as a backstop, just no longer the *only* thing
standing between you and a full disk.

## 4. Redeploy from GitHub

This already works — `Deploy()` re-syncs from `RepoURL`/`RepoRef` at the top
of every run, so calling `/run/{id}` again on a git-sourced project *is* a
redeploy. Nothing to build server-side. The only real gap here is the
frontend: there's no "redeploy" button distinct from the initial run — it's
the same call. If you want branch-switching (change which branch a project
tracks post-import), that's the smaller "two things" item from the earlier
dashboard doc (`RepoRef` set once, never updated) — worth a `PATCH
/projects/{id}/branch` if you want it, but it's not blocking a basic
redeploy flow, which is free today.

## 5. Storage and user limits

Splitting this into what's cheap now vs. what needs real infrastructure:

- **Orphaned anonymous projects.** A project staged via `/upload` and never
  claimed sits in Postgres and on disk (extracted zip under `./work`)
  forever. Needs a reaper: a periodic sweep (same shape as `Reconciler`)
  deleting projects where `user_id IS NULL AND created_at < now() -
  interval`, plus their extracted directory. Pick a TTL — an hour is
  generous for "upload, then go through signup."
- **Per-user disk quota.** The 20MB upload cap bounds one submission, not a
  user's total footprint. With a 2-project cap this is naturally bounded
  (2 × 20MB zips + their `node_modules` build output + build cache layers),
  but git imports have no size cap at all today — `GitSource.Sync` will
  clone whatever the remote hands it. Add a size check post-clone (or a
  shallow-clone depth limit) and reject/delete over some ceiling.
- **Stale `./work` directories generally.** Every deploy leaves an extracted
  source tree on disk (`ResolveProjectRoot` reads from it, and it's re-used
  on redeploy). A deleted project (once `DELETE /projects/{id}` exists) or a
  reaped anonymous one needs its directory removed, not just its DB row.
- **Build cache / Docker layer cache.** BuildKit's cache grows independently
  of image count — `docker builder prune` on a schedule, or a size-based
  cap, otherwise this is a second, separate disk-fill vector from the image
  leak in #3.

## 6. Error handling

Right now handlers mostly do `http.Error(w, err.Error(), status)` directly —
fine for you developing solo, not fine once the error text can be
`"db error: ERROR: duplicate key value violates unique constraint..."`
verbatim in front of a junior dev, which happens today whenever
`entities.MapPostgresError`'s fallback wrap surfaces. Two changes:

- **A response envelope.** `{"error": "human message"}` consistently, not
  bare text — trivial but every handler currently does
  `http.Error(w, ..., status)` (plain text body) which the dashboard-plan
  handlers I wrote also do, so this is a repo-wide pass, not just new code.
- **Don't leak internals.** Use-case errors meant for the client (validation,
  ownership, conflict) should stay as they are — they're already written as
  clear sentences. But anything that reaches a handler wrapped in `db error:
  %w` or similar should be logged server-side and replaced with a generic
  "something went wrong" for the response. Concretely: `isNotFound`/
  `isConflict` string-matching already act as an implicit allowlist of
  "safe to show" errors — formalize that instead of falling through to
  `err.Error()` as the default for anything unrecognized.

## What else I'd add

Ranked by how much it can actually hurt you, not by effort:

1. **Rate limiting on `/upload` and `/import/github` specifically, keyed by
   IP.** These are now unauthenticated (this session's change) — an
   anonymous endpoint that stages a build input with zero identity behind
   it is the cheapest possible abuse vector on the whole system, cheaper
   than the login-rate-limiting already on your list. This is more urgent
   than it was before the anonymous-submit flow existed.
2. **The reaper from #5 needs to exist before launch, not after** — you
   now have two anonymous-but-unauthenticated ways to put a file/repo clone
   on disk (upload, import), and no session gates them anymore. Combined
   with #1 missing, this is a disk-fill vector reachable by literally
   anyone with no account.
3. **Docker log-opts / rotation on containers**, since #1 in this doc adds a
   live-logs feature that reads from the daemon's log storage — unbounded
   container log growth is the same shape of problem as the image leak in
   #3, just for stdout instead of image layers.
4. Everything from the earlier punch list that's still open (Postgres
   creds, HTTPS/secure cookies, wildcard TLS, `/healthz`, structured
   logging, DB backups) still stands — this doc doesn't replace that one.

## Suggested build order

1. Image lifecycle fix (#3) — small, contained, stops active bleeding.
2. Reaper for orphaned anonymous projects (#5) + upload/import rate limiting
   (#1/#2 in "what else") — these two are joint because the anonymous flow
   is what makes both urgent now.
3. Max-2-projects cap (#2) — needs your call on the open question above
   first.
4. Live logs endpoint (#1) — self-contained, no dependency on the others.
5. Error envelope pass (#6) — mechanical, do last so it doesn't get
   re-litigated by every other change touching handlers this round.
