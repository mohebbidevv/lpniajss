# Tier 2 — Implementation Plan

> **Status:** §2.1–§2.6 are implemented. Only §2.7 (secrets/backups) remains. Two deviations from this plan were found
> during implementation and are recorded in §0.7 and §3.

Written against the code as it actually is on `dockermigration`, not against
the earlier fix document. Six things differ; those are listed first because
two of them mean less work than planned and one is a live bug the doc missed.

---

## 0. Corrections to the fix document

**0.1 — There is no `DeploymentDeploying` status.** `entities.DeploymentStatus`
(`internal/domain/entities/deployment.go:8`) has exactly five values: building,
running, stopped, failed, crashed. The sweeper's non-terminal set is therefore
**only `DeploymentBuilding`**. On the project side, `ProjectStatus` has
`StatusPending` and `StatusBuilding` — both non-terminal, both reachable when a
process dies mid-deploy.

**0.2 — `MaxBytesReader` is already there, and its error branch is dead.**
`upload_handler.go:30` already caps the body at 20MB. But line 34 compares
`err.Error()` against the string `"http: too large body"`, and Go's actual
message is `"http: request body too large"` (`net/http/request.go:1223`) — and
`FormFile` wraps it besides, so even the correct literal wouldn't match. **Every
oversized upload currently returns 400 "missing file field (multipart form:
file)" instead of 413.** Users hitting the cap get told their form is malformed.
Fix with `errors.As` against `*http.MaxBytesError`. **DONE** — verified: with the
old branch the oversized-upload test returns 400 "missing file field"; with
`errors.As` it returns 413.

**0.3 — Request IDs are half-built already.** `RequestLogger`
(`middlewares/logger.go:71`) already mints an ID, honours an inbound
`X-Request-ID`, and echoes it in the response header. What's missing is putting
it in the request **context**. §2.6 is much smaller than the doc implied.

**0.4 — `statusRecorder` is already correct.** First-write-wins on
`WriteHeader` and a forwarding `Flush` are both in place. Nothing to do.

**0.5 — `ProjectRepository` has no `ListAll`.** Image GC's periodic sweep needs
one. Adding a method to that interface breaks **every fake repository in the
test files** — see §8.

**0.6 — NEW: both SSE loops ignore client disconnect.** `run_handler.go:57` and
`get_project_logs_handler.go:59` are both a bare `for line := range logCh`. Neither
selects on `r.Context().Done()`, despite the comment at `run_handler.go:49`
claiming the request context cancels the stream — it only bounds `Execute`, not
the loop. Two consequences:

- A client that disconnects leaves the handler goroutine ranging the channel
  until the *producer* closes it, writing into a dead connection and discarding
  the error each time.
- **`server.Shutdown` will block for its entire timeout**, every time, because
  an SSE connection is never idle and Shutdown waits on non-idle connections.

This must be fixed as part of §2.1 or graceful shutdown is graceful in name only.

**DONE.** Both loops now select on `r.Context().Done()`. But the fix is *not*
symmetric, and the naive version of it introduces a worse bug than the one it
fixes — see the correction in §1 below.

**0.7 — CORRECTION to this plan's own §1: the two SSE producers have different
lifetimes.** Writing the obvious fix (select, then `return`) in both handlers
was verified to deadlock the build pool. Details in §1.

---

## 1. §2.1 — Graceful shutdown  ✅ DONE

### Current flow

`cmd/main.go` runs top to bottom: mkdirs → `config.LoadConfig` → `ConnectDB` →
`RunMigrations` → build repos → `buildDeployStack` → `NewDeployPipeline` →
define `processor` → `NewWorkerPool(4, processor, jobTimeoutFor(dc))` → `Start` →
`NewEventConsumer` + `go Run(ctx)` → `NewReconciler` + `go Run(ctx)` →
`NewAnonymousReaper` + `go Run(ctx, interval)` → `InitializeRoutes` →
`server.ListenAndServe()`.

`ctx` is `context.Background()` (line 33), so the three background goroutines
can never be told to stop. `ListenAndServe` blocks forever, so the three
`defer`s — `dbPool.Close`, `stack.Closer.Close`, `workerPool.ShutDown` — are
dead code. SIGTERM gets the runtime's default disposition: immediate process
death, no defers, no unwinding.

### Target flow

Replace `ctx := context.Background()` with `ctx, cancelRoot := context.WithCancel(...)`.
Delete all three `defer`s — they become explicit, ordered steps. Create
`shutdownCh := make(chan struct{})` near the top; it is the signal the SSE
handlers watch.

Move `ListenAndServe` into a goroutine that sends any non-`ErrServerClosed`
error to a buffered `serverErr chan error` (buffer 1, so the goroutine can't
leak if nobody reads). Then `signal.Notify` on a **buffered** channel (size 1 —
signal delivery is non-blocking and an unbuffered channel drops the signal if
the receiver isn't parked yet) for `SIGINT` and `SIGTERM`, and block on a
`select` over `serverErr` and the signal channel.

### The shutdown sequence, and why each step is where it is

0. **`close(shutdownCh)`** — before anything else. This releases the SSE
   handlers so they return, which is what lets step 1 finish instead of
   timing out. Closing a channel is the right primitive here: every reader
   observes it, and it needs no lock.
1. **`server.Shutdown(bounded ctx)`** — stops the listener, drains in-flight
   requests. Must be before the pool drain, or handlers keep enqueuing work
   you're about to abandon. On deadline expiry, fall through to
   `server.Close()`; a bounded context plus a forced close is the standard
   pairing, because Shutdown has no internal timeout of its own.
2. **`workerPool.Drain(45s)`** — the method now exists from Tier 1. In-flight
   builds get a window; new submissions already return `ErrPoolClosed`.
3. **`cancelRoot()`** — stops `EventConsumer.Run`, `Reconciler.Run`,
   `AnonymousReaper.Run`. After step 2, because the pipeline builds contexts
   derived from this one.
4. **Sweep abandoned deployments** (§2.2) on a *fresh* bounded context —
   `ctx` is cancelled now, so reusing it would fail every query instantly.
5. **`stack.Closer.Close()` then `dbPool.Close()`** — last, and in that order,
   because step 4 needs the DB.

Imports to add to `main.go`: `errors`, `os/signal`, `syscall`.

### The SSE change

`ServerConfig` (`internal/infrastructure/http/server.go:118`) gains a
`Shutdown <-chan struct{}` field. `InitializeRoutes` passes it into
`InitializeRunProjectHandler` and `InitializeGetProjectLogsHandler`, which store
it on the handler struct.

Both loops change from `for line := range logCh` to a `for` + `select` over
three cases: a receive from `logCh` (with the `ok` form, so a closed channel
ends the loop), `<-r.Context().Done()`, and `<-h.Shutdown`.

Watch the receive form: `case line, ok := <-logCh` — without `ok` a closed
channel spins the select at 100% CPU forever.

### CRITICAL: what to do on the exit cases is NOT the same in both handlers

The two channels have completely different producers, and treating them alike
deadlocks the build pool. This was verified empirically, not reasoned about:

**`get_project_logs_handler` — plain `return` is correct.** Its channel comes
from `Runtime.Logs(r.Context(), ...)`, and `scanLines`
(`dockerrun/logs.go:88`) selects on that same context on every send. Cancel the
request and the producer aborts its sends, `demux` returns, the channel closes.
Nothing is left blocked.

**`run_handler` — a plain `return` stalls a deploy worker.** Its channel comes
from the `LogRegistry`, and the producer is `DeployPipeline.streamLog`
(`deploy_pipeline.go:308`) running on a **worker goroutine under the job
context** — which has nothing to do with the request. `streamLog`'s send only
aborts when the *job* context ends, i.e. after `jobTimeoutFor(dc)` ≈
BuildTimeout 900s + ReadyTimeout 90s + 5min slack ≈ **21 minutes**.

So on disconnect: the 64-slot buffer (`run_project.go:51`) fills, then
`streamLog` blocks, and that worker is dead for up to 21 minutes holding 1 of 4
slots. **Four disconnected browser tabs deadlock the entire build pipeline.**
That is strictly worse than the leak being fixed — today's `range` at least
keeps draining.

The fix is to hand the channel to a drain goroutine before returning: keep
receiving and discarding until the producer closes it, which the worker's
`processor` (`main.go:83-87`) does unconditionally once `Deploy` returns. That
decouples "this HTTP response is finished" from "the producer has finished",
which is exactly what lets `Shutdown` complete.

**DONE** for the disconnect case. The `h.Shutdown` case still needs adding when
§2.1 lands, and it needs the same drain hand-off in `run_handler` — the
producer doesn't care *why* the consumer left.

The regression test (`run_handler_test.go`) asserts both halves, and was
verified to fail against the old code (handler never returns) and against the
naive fix (producer blocks at exactly send 64).

### Known residue, worth accepting for now

`processor` in `main.go:83` closes the project's log channel via `defer` and
deletes the registry entry. Jobs still sitting in the queue when Drain gives up
never run, so their `LogRegistry` entries leak and their SSE clients hang until
`shutdownCh` releases them. Acceptable: the process is exiting. Don't build a
registry-drain step for it.

---

## 2. §2.2 — The abandoned-deployment sweeper  ✅ DONE

### Why it's needed

`Reconciler.Run` (`internal/application/reconcile.go:46`) queries
`ListByStatus(ctx, entities.DeploymentRunning)` and nothing else. A row parked
at `building` is invisible to it, so nothing in the system will ever advance
that row. The user watches a spinner forever; the only exit is manual SQL.

Note that `Reconciler.Run` is a **one-shot**, not a loop, despite the name — it
executes once at startup from `go reconciler.Run(ctx)` (`main.go:110`).

### Shape

New file `internal/application/deployment_sweeper.go`, one exported function:

`FailAbandonedDeployments(ctx, deploymentRepo, projectRepo, olderThan time.Duration) (int, error)`

A free function, not a struct — it has no state and two callers with different
lifecycles. It lists `DeploymentBuilding` rows, skips any newer than
`time.Now().Add(-olderThan)` when `olderThan > 0`, and for each survivor calls
`DeploymentRepo.SetFailed(ctx, id, reason, nil)` then
`ProjectRepo.UpdateStatus(ctx, d.ProjectID, entities.StatusFailed)`.

`olderThan == 0` means "all of them", which is correct at both call sites with a
single control-plane instance: a fresh process owns no in-flight builds, and at
shutdown this process was the only one that could have owned them. A grace
period only becomes necessary if you ever run two instances.

Continue past per-row errors rather than aborting — one bad row must not block
the rest of the sweep. Return the count actually swept so the caller can log
something meaningful.

### Wiring

Two call sites:

- **`Reconciler.Run`, as the very first statement**, before the
  `ListByStatus(DeploymentRunning)` call at `reconcile.go:46`. First, so a stuck
  row can't be seen by both the sweep and the running-deployment pass.
- **`main.go` shutdown step 4.**

### Migration

`ListByStatus` currently scans `deployments`. Add a composite index on
`(status, created_at DESC)`.

Build it `CONCURRENTLY` so it doesn't lock the table — but `CREATE INDEX
CONCURRENTLY` cannot run inside a transaction block. Put it in its own migration
file with no other statements, and if `golang-migrate` still wraps it, drop the
`CONCURRENTLY` (the table is small enough today that a brief lock is harmless)
or run it by hand and mark the migration applied.

---

## 3. §2.3 — Server timeouts and the upload 413  ✅ DONE

**DEVIATION — `/stop` and `DELETE /projects/{id}` must NOT take the 15s bound.**
This plan listed both as ordinary JSON routes. They are not: `stopTimeoutSeconds`
is 10 on its own (`deploy_pipeline.go:13`), and delete additionally removes an
image and `os.RemoveAll`s a source tree. Worst case runs past 15s.

The danger is not a slow response — it is that `http.TimeoutHandler` **cancels
the request context** when it fires, and both use cases do their teardown on
that context. A truncated delete leaves the Caddy route removed, the container
half-stopped and the DB never updated. Both now use a separate 60s
`boundedLong`, which keeps a ceiling without being able to sever a teardown
mid-flight.

### Timeouts

Four fields on the `http.Server` literal in `main.go:117`:
`ReadHeaderTimeout` 10s (slowloris — the important one), `ReadTimeout` 60s
(stalled request bodies; `/upload` takes multipart zips), `IdleTimeout` 120s
(keep-alives holding an fd and a goroutine each), `MaxHeaderBytes` 1MB.

**Do not set `WriteTimeout`.** It is a deadline on the entire response measured
from the end of the request headers, not an idle timeout. Any value kills both
SSE endpoints at exactly that many seconds, mid-deploy, with a truncated
connection and no error on either side. Your build budget is 900s.

### Per-route bounds instead

Add a small helper in `server.go` wrapping `http.TimeoutHandler` at ~15s, and
apply it in `InitializeRoutes` to the ordinary JSON routes: `/projects`,
`/projects/{projectID}`, `DELETE /projects/{projectID}`, `/projects/{projectID}/env`,
`/projects/{projectID}/slug`, `/projects/{projectID}/deployments`, `/auth/*`,
`/stop/{projectID}`.

**Never** wrap: `/run/{projectID}` (SSE), `/projects/{projectID}/logs` (SSE),
`/upload` (large body), `/import/github` (clones a repo inline),
`/projects/{projectID}/rollback` (starts a container and waits for readiness —
`ReadyTimeout` alone is 90s).

Second reason SSE must be excluded: `TimeoutHandler` buffers the whole response
in memory before writing it, so it would suppress every event until the handler
returned.

### The 413

At `upload_handler.go:34`, replace the string comparison with `errors.As(err,
&maxErr)` against `*http.MaxBytesError`. Keep the `MaxBytesReader` wrap where it
is — before `FormFile`, which is correct: it makes the server stop reading and
close the connection rather than politely accepting the whole body and then
rejecting it.

While there: 20MB is tight for a `node_modules`-free zip but not generous.
Consider lifting it to 100MB and moving the constant to `ServerConfig`.

---

## 4. §2.4 — Health endpoints  ✅ DONE

### The distinction

`/healthz` (liveness) answers "is this process wedged?" — a bare 200 that
touches **nothing** external. If liveness checks Postgres, a five-second
database blip makes your supervisor kill a perfectly healthy process, turning a
hiccup into a restart cascade.

`/readyz` (readiness) answers "can this serve traffic right now?" — it checks
dependencies. A 503 pulls you out of rotation without killing you, so you
recover on your own when the dependency does.

### `Runtime.Ping`

`repository.Runtime` has no reachability probe. Add
`Ping(ctx context.Context) error` to the interface
(`internal/domain/repository/runtime_repo.go`).

- `dockerrun`: delegate to `r.cli.Ping(ctx)` — `DockerRuntime` already holds
  `cli *client.Client` (`runtime.go:21`). Note the Docker SDK's `Ping` returns
  `(types.Ping, error)`; discard the first value.
- `hostexec`: return `nil` — there is no daemon to reach.

`dockerrun` has `var _ repository.Runtime = (*DockerRuntime)(nil)` at
`runtime.go:25`, so a missing method fails the build immediately, which is what
you want.

### Handler

New `handlers/health_handler.go` with a struct holding the `*pgxpool.Pool`, the
`repository.Runtime`, a version string, and a start time; methods `Live` and
`Ready`. `Ready` builds its own ~2s context — a probe must never inherit a slow
client's deadline or block on a wedged dependency — and reports per-dependency
status in the body so a failure is diagnosable without shelling in.

### Registration

In `InitializeRoutes`, register these **outside** both `cors(...)` and
`requireAuth(...)`. A probe has no cookie and no browser origin; wrapping them
in either makes them fail for the wrong reason.

`/internal/stats` (the worker pool's `GetStats`, now carrying `queue_depth`,
`in_flight`, `rejected` and `breaker_open` from Tier 1) must **not** be public —
queue depth and failure counts are reconnaissance. Gate it behind a shared token
from config, compared with `crypto/subtle.ConstantTimeCompare` (a plain `==`
leaks the token a byte at a time through response timing), and return **404, not
401**, so you don't confirm the endpoint exists.

`ServerConfig` gains `InternalToken string` and `Version string`.

Finally: skip `/healthz` and `/readyz` in `RequestLogger`. At a 10-second probe
interval they'd be ~99% of your log volume.

---

## 5. §2.5 — Image garbage collection  ✅ DONE

### Prerequisite: `ProjectRepository.ListAll`

Add `ListAll(ctx) ([]*entities.Project, error)` to the interface and implement
it in `postgres/project_repo_impl.go`. See §8 for the fakes this breaks.

### Shape

New file `internal/application/image_gc.go` — a struct (it has dependencies and
a configured retention, unlike the sweeper) with `ProjectRepo`,
`DeploymentRepo`, `Builder`, and `Retain int`. Two methods:

- `CollectProject(ctx, projectID)` — no error return. GC is best-effort and must
  never fail a deploy that has already succeeded; log and move on.
- `Run(ctx, interval)` — the periodic sweep, started from `main` in a goroutine.

### The rules `CollectProject` must respect

`DeploymentRepo.ListByProject` returns newest-first, which is the ordering the
algorithm depends on. Walk it and:

- **Deduplicate by `ImageRef` before counting.** Several deployment rows share
  one image (a rollback creates a new row pointing at an old image). Counting
  rows instead of distinct images silently shrinks your retention window.
- **Protect the live image absolutely.** Look up
  `DeploymentRepo.GetCurrentForProject` and never remove that `ImageRef`
  regardless of age. Removing it would break the running container's ability to
  restart and leave Docker's own reference counting as the only thing between
  you and an outage.
- **Only reclaim terminal deployments.** Skip rows still `DeploymentRunning` or
  `DeploymentBuilding` — they may be about to reference the image.
- **Treat removal failures as benign.** `DockerImageBuilder.RemoveImage`
  (`dockerbuild/builder.go:286`) already swallows not-found; a *conflict* means
  a container still holds the image, which the next sweep will catch. Log at
  debug, continue.

`Retain = 5` is a direct disk-for-rollback-window trade. Make it a config field.

### Wiring into the pipeline

`DeployPipeline` gains an `ImageGC *ImageGC` field, set in
`NewDeployPipeline`. Call `CollectProject` at the very end of `Deploy` — after
the route flip and the old-container teardown — in a goroutine, so the user's
deploy doesn't wait on disk reclamation.

Use `context.WithoutCancel(ctx)` for that goroutine: it keeps the values (the
request ID from §2.6) while shedding the cancellation, which is exactly right
for fire-and-forget work started from a request. A plain `ctx` would be
cancelled the moment the handler returns; a `context.Background()` would lose
the correlation.

Nil-check the field so existing tests that construct a pipeline without a GC
keep passing.

### Build cache

Retention on tagged images does nothing for Docker's build cache, which is also
unbounded. Prune with an **age filter** (~168h), never a bare prune-all — that
would discard the warm layer cache that makes incremental builds fast.

Don't put this on the `ImageBuilder` interface; `hostexec` has no build cache
and would need a meaningless no-op. Define a small optional interface
(`interface{ PruneBuildCache(ctx) error }`) in the application layer and
type-assert the builder inside `ImageGC.Run`. That keeps the port honest.

### Disk guard

GC is reactive; a pre-flight check is what turns "every build fails
cryptically" into one clear message. Before starting a build in the pipeline,
`syscall.Statfs` on the Docker data root and compare `Bavail * Bsize` against a
floor (~5GiB). Stream a plain-language message to the user's log and fail the
deploy early.

If `Statfs` itself errors, **allow the deploy** — never block users on your own
instrumentation bug.

### Config additions

`ImageRetentionCount`, `ImageSweepIntervalMinutes`, `MinBuildDiskGB` on
`RuntimeConfig`, with defaults in `applyDefaults`.

---

## 6. §2.6 — Structured logging and correlation  ✅ DONE

### What already exists

`RequestLogger` mints and echoes `X-Request-ID` (`logger.go:74-79`). The pretty
coloured single-line output is genuinely good for development — keep it, and
select between it and JSON on a config flag rather than deleting it.

### New pieces

A `internal/infrastructure/logging` package with three functions:
`New(level, format string) *slog.Logger` (JSON in prod, text in dev, `AddSource`
only at debug, and call `slog.SetDefault` so stragglers land in the same
stream), plus `Into(ctx, *slog.Logger) context.Context` and
`From(ctx) *slog.Logger`. `From` must **never return nil** — fall back to
`slog.Default()`; a missing logger is a wiring bug, not a reason to nil-panic
mid-incident.

A `RequestID` middleware in `middlewares/` that does what `RequestLogger`
already does *plus* stores the ID in the context, with a `RequestIDFrom(ctx)`
accessor. Then strip the ID-minting out of `RequestLogger` and have it read from
the context instead. Chain order in `main.go:56`: `RequestID` outermost, then
`RequestLogger`, then the mux — the logger must see the ID the middleware set.

### The async boundary — the part that actually matters

The request context dies when the handler returns, but the deploy it triggered
runs for minutes afterwards on a worker goroutine. So the request ID cannot be
read from a context on the worker side. **Add `RequestID string` to
`queue.Job`** (`internal/queue/types.go`) and populate it in
`run_project.go:54`, where the job is submitted with the request context still
live.

The `processor` closure in `main.go:83` then builds a logger with `request_id`,
`job_id` and `project_id` bound, and stashes it via `logging.Into(ctx, log)`
before calling `pipeline.Deploy`. Every layer below does a one-line
`logging.From(ctx)` and inherits those fields.

That is the whole payoff: one query, `request_id="…"`, returns the complete
story of a single deploy across handler, queue, pipeline, builder, runtime and
event consumer.

### Call-site sweep

Replace `log.Printf` in: `application/reconcile.go`, `event_consumer.go`,
`anonymous_reaper.go`, `deploy_pipeline.go`, `queue/workerpool.go`,
`cmd/main.go`. Config gains `LogLevel` and `LogFormat` on `ServerConfig`.

---

## 7. §2.7 — Secrets and backups  ⬜ NOT STARTED

In `config.LoadConfig`, **after** `utils.OpenJSON` and **before** the
required-field check at `config.go:114`, apply environment overrides for
`GOLAUNCH_DB_PASSWORD`, `GOLAUNCH_DB_HOST`, and `GOLAUNCH_INTERNAL_TOKEN`.

Environment wins over file deliberately: the file carries structure and
defaults, secrets come from the process environment — not committable, not
exposed by a stray `cat` of the repo, and the format systemd, Docker secrets and
Kubernetes all already speak. Commit the example config with an empty password;
the existing check at `config.go:114` already makes an empty password fatal at
boot, which is the behaviour you want.

Backups: `pg_dump --format=custom` on a 6-hour cron (custom format so
`pg_restore` can do selective, parallel restores, compression built in), pushed
**offsite**, local copies pruned after ~14 days. A backup on the same disk as the
database is a second copy of the thing that's about to fail.

Two non-negotiables people skip: **test the restore** monthly into a scratch
database and confirm row counts, and **alert on absence** — a job that has
silently failed for three weeks is worse than no backup, because you believed
you had one.

---

## 8. What breaks at compile time

Two interface changes ripple:

**`repository.Runtime` gains `Ping`** → `dockerrun.DockerRuntime`,
`hostexec.HostExecRuntime`, and every fake runtime in
`application/deploy_pipeline_test.go` and `application/rollback_deployment_test.go`.

**`repository.ProjectRepository` gains `ListAll`** → `postgres` impl plus every
fake project repo in `application/auth_test.go`,
`application/deploy_pipeline_test.go`, `application/rollback_deployment_test.go`.

The fakes need a one-line stub each. If that churn annoys you, the alternative
is to declare a narrower interface at the point of use — `ImageGC` only needs
`ListAll`, so it could take an `interface{ ListAll(...) }` instead of the whole
repository. That's the more Go-idiomatic move and it keeps the fat interface
from growing, but it's a judgement call about how much indirection you want.

Also changing shape: `ServerConfig` (+`Shutdown`, `InternalToken`, `Version`,
`LogLevel`, `LogFormat`), `RuntimeConfig` (+GC fields), `queue.Job`
(+`RequestID`), `DeployPipeline` (+`ImageGC`), and the run/logs handler structs
(+`Shutdown`).

---

## 9. Suggested order

1. **§2.2 sweeper** — self-contained, no interface changes, and it cleans up the
   zombie rows your restarts have already created. Immediate visible win.
2. **§2.1 graceful shutdown + the SSE disconnect fix** — do these together;
   shutdown without the SSE fix just times out every time.
3. **§2.3 timeouts + the 413** — small, mechanical, no wiring.
4. **§2.4 health** — first interface change (`Ping`), so do it when you're ready
   to touch the fakes.
5. **§2.6 logging** — touches the most files; do it once the above are stable so
   you aren't rebasing a wide diff.
6. **§2.5 image GC** — second interface change, most new logic, and the least
   urgent of the group until disk actually starts filling.
7. **§2.7 secrets + backups** — no Go changes beyond a handful of env lookups;
   mostly ops work you can do in parallel with any of the above.

## 10. How to verify each

- **Sweeper:** insert a `building` deployment row by hand, restart, confirm it
  lands `failed` and its project follows.
- **Shutdown:** start a deploy, `kill -TERM` mid-build, confirm the process
  exits within the budget (not instantly, not hung) and leaves no `building`
  row. Open an SSE stream first to prove step 0 works.
- **Timeouts:** `curl` with a deliberately slow header write for slowloris;
  upload a 30MB zip and assert 413 rather than 400.
- **Health:** stop Postgres, confirm `/readyz` goes 503 while `/healthz` stays
  200. That asymmetry is the whole point.
- **Logging:** grep one `request_id` and confirm it appears in handler, queue
  and pipeline lines for the same deploy.
- **Image GC:** deploy seven times, confirm five images remain and the live one
  is among them.


---

## 11. Implementation notes (added after the fact)

**Not verified end to end.** There is no Postgres or Caddy running in this
environment, so the shutdown sequence has never been exercised against a real
signal with a real build in flight. Everything below is covered by unit tests
and by `go vet` / `go test -race`, but §10's manual checks — especially the
`kill -TERM` mid-build one — still need running against a live stack.

**`ProjectRepository.ListAll` was NOT added to the interface.** `ListAll` is a
method on the concrete `postgres.ProjectRepository` only, and `ImageGC` depends
on a narrow `projectLister` interface declared at its own point of use. One
caller's requirement therefore forced no stub into any fake.

**The image GC guard rails were ablation-tested**, not just asserted: removing
the live-image protection, the dedupe-by-image-ref, or the terminal-status
check each makes a specific test fail. All three are load-bearing.

**`version`** is a package-level `var` in `cmd/main.go` rather than a config
field, so it can be stamped at build time:
`go build -ldflags "-X main.version=$(git rev-parse --short HEAD)"`.

**Request-ID context helpers live in `internal/infrastructure/logging`**, not
in the HTTP middleware package. `run_project.go` needs to read the ID when it
builds a `queue.Job`, and having the application layer import an HTTP package
for that would have been the wrong direction.

**New config keys** (all defaulted, so an existing `configuration.json` keeps
working): `server.internal_token`, `server.log_level`, `server.log_format`.
Leaving `internal_token` unset makes `/internal/stats` 404 for everyone, which
is the intended fail-closed behaviour.
