# golaunch — Production Fixes

Every item from the three tiers: what is actually broken at the mechanism
level, why the obvious fix is wrong where it is wrong, and what to change.
No code — this is the design, the implementation is mechanical once the
reasoning is settled.

---
---

# TIER 1 — Live correctness bugs

---

## 1.1 The Caddy read-modify-write race

### The mechanism

`RegisterRoute` does three network round trips with no atomicity: GET the
whole routes array, mutate the slice in Go memory, PATCH the whole array
back. Two deploy workers finishing at the same time interleave — both GET
the same starting array, both mutate their own copy, both PATCH. The second
PATCH overwrites the first worker's route entirely.

The victim project built successfully, started successfully, passed
`WaitReady`, wrote `running` to Postgres, and told the user "live at
a.domain". It 502s forever. **No error is logged anywhere**, because from
that worker's point of view every single call returned 200.

This is the worst failure shape in the codebase: silent, persistent, and
invisible in logs. It is reachable right now — you have 4 workers, and every
deploy ends in a route flip.

### Why a mutex is only half a fix

I told you "add a mutex, ten minutes." Working through it, that is the wrong
answer. A `sync.Mutex` serializes the three calls within one process, which
does close the bug for today — it also covers the `Reconciler` goroutine,
since that is the same process. But it does not cover a second control-plane
instance, or anyone touching the Caddy admin API by hand during a deploy.

More importantly it is still O(all routes) of JSON on the wire per deploy.
At 1000 projects you are PATCHing a megabyte of JSON to flip one route.

### The correct fix: Caddy `@id` addressing

Caddy's admin API lets any JSON object carry an `@id` field. Once set, that
object is addressable directly at `/id/<the-id>`, independent of its position
in the array. `PUT /id/x` replaces exactly that object; `DELETE /id/x`
removes exactly that object. Both are single atomic server-side operations.
No read, no whole-array write, no race, no O(n) payload.

**The key decision: key the `@id` on `project.ID`, not on the slug.** The
slug is mutable (rename); the project ID is not. Keying on the ID makes
rename a plain replace of the same object with a different host matcher.

### What this changes structurally

- `RegisterRoute` becomes: try `PUT /id/<routeID>` (the common case — a
  redeploy or rollback is re-registering a project that already has a route).
  Only on 404 (first ever deploy) fall back to inserting into the array.
- The insert should `POST` to `.../routes/0`, which inserts at index 0 rather
  than replacing the array — so even the fallback path does not clobber
  concurrent writers the way a whole-array PATCH does.
- `RemoveRoute` becomes a single `DELETE /id/<routeID>`, treating 404 as
  success — stop, delete, and crash-cleanup all call it on paths where the
  route may legally be absent already.
- **`RenameRoute` disappears entirely.** It existed to avoid a window where
  neither hostname resolved. With project-keyed routes that window cannot
  exist: rename is just re-registering with the new slug and the same dial
  target, one atomic replace. Delete it and `matchesHost` with it.
- Add a `RouteExists` check so the Reconciler can skip pointless rewrites at
  startup.

Two things worth fixing in the same pass: the client uses
`http.DefaultClient`, which has **no timeout** — a wedged Caddy admin socket
hangs a deploy worker forever, and a hung worker is 25% of your build
capacity. And the response body is not always drained, so connections are
torn down rather than reused.

### The call-site migration

`RegisterRoute` and `RemoveRoute` gain a `projectID` parameter. Every caller
already has the project in hand, so it is mechanical: `deploy_pipeline.go`,
`rollback_deployment.go`, `reconcile.go`, `stop_project.go`,
`delete_project.go`, `event_consumer.go`, and `rename_project.go`.

### The one-time migration hazard

Routes already in Caddy have no `@id`. The first `PUT /id/...` after this
ships will 404 and fall through to the insert path — which **adds a second
route for the same hostname**. The older, unidentified one wins, because
Caddy matches in order and yours is terminal. The project keeps serving the
*stale* upstream, which is exactly the bug you were fixing.

Flush the routes array once (PATCH an empty array to the admin API) in the
same window you restart the process. The Reconciler re-registers everything
that should be live within seconds of boot.

---

## 1.2 The `WorkerPool.stats` data race

### The mechanism

`wp.stats.submitted++` compiles to load / add / store — three separate
machine operations, not one. Four workers running it concurrently on
different cores lose updates: two loads read the same value, both store
value+1, one increment vanishes. `GetStats()` reads all three fields with no
synchronization at all, so it can observe a partially-updated struct.

`go test -race` flags this today. It is a genuine race under the Go memory
model, not a theoretical one.

### Two secondary bugs in the same six lines

- **`failed` increments on every attempt, not on terminal failure.** A job
  that fails twice then succeeds records `failed=2, completed=1`. Your
  failure rate is inflated by exactly your retry rate — the metric becomes
  unusable precisely when you most need it, during an incident.
- **`submitted` is incremented at the end of `Process`, not in `Submit`.**
  It is a completion counter wearing a misleading name, and it double-counts
  retried jobs.

### The fix

`sync/atomic` typed values, not a mutex — these are independent counters with
no invariant between them, so per-counter atomics are both faster and
simpler. Widen to `int64` so they cannot wrap on a long-lived process.

Split the counters so they mean something: `submitted` (incremented in
`Submit`, once per accepted job), `attempts` (every processor invocation,
retries included), `completed`, `failed` (terminal only), `retried`, and
`rejected` (Submit refused — buffer full, pool closed, or breaker open).

The distinction that matters: **`attempts` counts work done, `failed` counts
jobs lost.** That gives you a real invariant to assert on —
`submitted == completed + failed + in-flight + queued`.

`rejected` is worth calling out separately: it is users being turned away,
and right now that is invisible everywhere in the system.

---

## 1.3 `ShutDown()` panics, and the retry storm

### Why `close(wp.jobs)` is wrong

Closing a channel is only safe when you can prove no sender remains. You
cannot, because there are three independent senders:

1. HTTP handlers, via `run_project.go`
2. `Reconciler`, at startup
3. **`EventConsumer.maybeRestart`, from inside a `time.AfterFunc` callback**
   — a goroutine that fires up to 5 seconds after the event that scheduled
   it, with no relationship to the shutdown sequence

Sender 3 is the killer. Close-then-send is `panic: send on closed channel` —
not an error you can handle, a process abort.

Today this is masked only because `ListenAndServe` blocks forever and
`ShutDown` is a `defer` that never runs. **The moment you add graceful
shutdown, you arm this panic.** The two fixes must land together.

The correct pattern is to **never close the job channel.** Gate sends behind
a `closed` flag under an `RWMutex`: `Submit` takes the read lock (cheap,
concurrent — four workers never contend), `ShutDown` takes the write lock
once. After the flag is set, `Submit` returns `ErrPoolClosed`. A late
`AfterFunc` gets a clean error, not a panic. Workers exit on context
cancellation instead.

### Retry amplification — the real cost

The backoff is `time.Sleep` **executing on the worker goroutine**. With 4
workers, four simultaneously-retrying jobs mean *zero* build capacity for up
to 6 seconds while the queue keeps filling. That is the exact opposite of
what you want during an incident: load is rising and you have voluntarily
gone to zero throughput.

Second problem: two independent retry systems compound. The pool retries 3×.
`EventConsumer` independently restarts a crashed project 3× — and each of
those is a fresh `Submit` that gets its own 3 retries. Worst case is roughly
**12 build attempts per project**.

Now imagine the shared cause: disk full, Docker daemon wedged, npm registry
down. Every project in the system enters this simultaneously. You generate
peak load at the exact moment the system is least able to serve it. That is
the classic retry storm, and it is how a partial outage becomes a total one.

### Three fixes

1. **Move the wait off the worker.** Schedule the requeue on a timer
   goroutine; the worker immediately picks up the next job.
2. **Full jitter.** Draw the backoff uniformly from `[0, 2^n × base)` rather
   than using a fixed exponential. Plain exponential backoff *synchronizes*
   retries — every job that failed on the same shared cause wakes at the same
   instant and hits the recovering dependency together.
3. **A circuit breaker.** Count consecutive terminal failures across all
   jobs; past a threshold (~10), refuse new submissions for a cooldown
   (~30s). Any single success resets the count, so it only fires on a
   genuinely systemic fault, not on a run of users pushing broken code.
   This is the piece that actually stops the storm: it converts "hammer a
   dead dependency 12× per project" into "fail fast, one probe per cooldown".

Retries must **bypass** the breaker check — refusing an in-flight job's retry
throws away work already done.

### Two more bugs visible once you are in there

- `Process` derives its timeout context from `context.Background()`, not from
  the pool's context. A cancelled pool cannot abort in-flight work, so a job
  can outlive shutdown by the full 20-minute timeout.
- A job failing *because* the pool is shutting down currently burns a retry
  and would trip the breaker. Check the pool context before counting it as a
  failure.

### Drain, and why the ordering matters

`Drain(timeout)` should mark the pool closed **first** so nothing new enters,
then wait for in-flight jobs and pending retry timers, and only cancel the
context if the deadline passes. Cancelling first would abort builds that were
about to succeed.

The wait has two arms: worker goroutines, and the retry timers sleeping
outside the pool. Miss the second and shutdown races a pending requeue.

### The EventConsumer half

Cap the total. Drop `defaultMaxRestarts` from 3 to 2, and make the restart
delay grow with the crash count (5s → 10s → 20s, capped at ~2 minutes) so a
crash-looping app backs off instead of hammering. Combined with the pool's 3
retries, worst case falls from ~12 attempts to ~6, and the breaker caps the
systemic case regardless.

`Submit` can now fail — pool closed, queue full, breaker open. `maybeRestart`
currently ignores its return value, which would drop restarts silently.

---
---

# TIER 2 — You cannot operate it without these

---

## 2.1 Graceful shutdown

### What actually happens today on SIGTERM

`main` ends in `ListenAndServe()`, which blocks forever, and there is no
`signal.Notify`. So SIGTERM is handled by the Go runtime's default
disposition: **immediate process death.** Every `defer` in `main` — the DB
pool close, the Docker client close, the worker pool shutdown — is never
executed. `defer` runs on normal return or panic unwinding, not on signal
termination.

A SIGTERM while a build is running leaves:

| Thing | State |
|---|---|
| The build | Docker keeps building; nobody will ever read the result |
| The deployment row | `status = 'building'`, **forever** |
| The project row | `status = 'building'`, forever |
| A container started but not yet routed | Running, no Caddy route, invisible |
| The SSE log channel | Client hangs until its own TCP timeout |
| The pgx pool | Connections abandoned; Postgres reaps them on its own timer |

The deployment row is the one that never heals — see §2.2. **Every
deploy-time restart leaks one zombie row**, including every deploy of your
own code.

### The sequence, in order, with reasons

Order matters and the steps are not interchangeable:

1. **Stop accepting HTTP** (`server.Shutdown`). First, or you keep enqueuing
   work you are about to abandon.
2. **Drain the worker pool.** In-flight builds get a window. After (1), or
   new submissions keep arriving.
3. **Cancel the root context.** Stops `EventConsumer`, `Reconciler`, the
   reaper. After (2) because the pipeline uses contexts derived from it.
4. **Mark abandoned rows failed.** Anything still building or deploying gets
   a terminal status, so the UI shows a real error instead of an eternal
   spinner.
5. **Close the Docker client, then the DB pool.** Last, because (4) needs
   the DB.

This requires `ctx` in `main` to come from `context.WithCancel` rather than
`Background`, and the existing unordered `defer`s to be replaced by this
explicit sequence.

### The SSE gotcha

`http.Server.Shutdown` waits for **idle** connections, and an SSE stream is
never idle. `/projects/{id}/logs` will hold shutdown open until its own
deadline. So the shutdown context must be bounded and must fall through to a
forced `Close()` when it expires.

Better: have the SSE handler select on a server-wide "shutting down" channel
alongside its existing cases, so the graceful path actually completes instead
of always timing out.

---

## 2.2 Zombie `building` rows — the sweeper

### The bug I under-reported

`Reconciler.Run` lists deployments by status — and it only ever queries
`DeploymentRunning`. There is **no case for `DeploymentBuilding` or
`DeploymentDeploying`**. A row stuck in a non-terminal state is therefore
invisible to every recovery path you have. The user sees a permanent
spinner; the only exit is manual SQL.

Shutdown is just one way to produce one. A panic, a `kill -9`, an OOM kill,
or a power loss all do the same — and none of those get to run any shutdown
code at all. That is why this needs to run **at startup**, not only on the
way out.

### The sweeper

A function that lists every deployment in a non-terminal state, marks it
failed with a reason like "deploy was interrupted (control plane restarted)",
and drops its project out of the building state.

The age threshold: with a single control-plane instance, "all of them" is
correct at both startup and shutdown — a fresh process by definition owns no
in-flight builds, and at shutdown this process was the only one that could
have owned them. A grace period is only needed if you ever run more than one
instance.

Wire it into `Reconciler.Run` as the **first** step, before it lists running
deployments, so a stuck row cannot be double-processed. Call it again in the
shutdown sequence.

### An index you will want

`ListByStatus` currently scans. Add a composite index on
`(status, created_at DESC)` on `deployments`, built `CONCURRENTLY` so it does
not lock the table. Note that `CONCURRENTLY` cannot run inside a transaction
block, so it needs its own migration file with no wrapping BEGIN.

---

## 2.3 HTTP server timeouts — and the one that will break you

| Setting | Value | Defends against |
|---|---|---|
| `ReadHeaderTimeout` | 10s | **Slowloris** — connections dribbling header bytes forever. The single most important one. |
| `ReadTimeout` | 60s | A slow or stalled request *body* — matters because `/upload` accepts multipart zips. |
| `IdleTimeout` | 120s | Keep-alive connections held open doing nothing, one fd and one goroutine each. |
| `MaxHeaderBytes` | 1MB | Memory exhaustion via enormous headers. |

### Why `WriteTimeout` is absent

`WriteTimeout` is a deadline on the **whole response**, measured from the end
of the request headers. It is not an idle timeout. Setting it to any value
kills every SSE stream at exactly that many seconds, mid-deploy, with a
truncated connection and no error on either side.

You have two long-lived streaming endpoints, and both legitimately run for
the length of a build — which your own config budgets at 900 seconds.

So: no global `WriteTimeout`. Bound the non-streaming handlers individually
with `http.TimeoutHandler` (~15s for ordinary JSON endpoints). Never wrap the
log stream, `/upload`, or `/import/github` with it — and note that
`TimeoutHandler` **buffers the entire response in memory** before writing,
which is a second independent reason it must not touch SSE.

### The upload body limit you are also missing

`ReadTimeout` bounds *time*, not *size*. Nothing currently stops a 50GB
upload from filling the disk. Wrap the request body in
`http.MaxBytesReader` (~100 MiB) before parsing the multipart form, and map
the resulting `*http.MaxBytesError` to a 413.

`MaxBytesReader` specifically — rather than a manual size check — is what
makes the server stop reading and close the connection, instead of politely
accepting all 50GB and then rejecting it.

---

## 2.4 Health endpoints

### `/healthz` vs `/readyz` — the distinction that matters

They answer different questions and must never share a handler:

- **`/healthz` (liveness):** "is this process wedged?" Answer with a bare
  200 and touch **nothing** external. If liveness checks the database, a
  brief Postgres blip makes your supervisor kill an otherwise perfectly
  healthy process — turning a 5-second hiccup into a restart cascade.
- **`/readyz` (readiness):** "can this process serve traffic right now?"
  This one *does* check dependencies. A failing readiness check pulls you out
  of the load balancer without killing you, so you recover on your own when
  the dependency does.

Getting these backwards is one of the most common ways a deployment amplifies
a small outage into a total one.

Readiness needs its own short budget (~2s) rather than inheriting a slow
client's context, and should report per-dependency status so the failure is
diagnosable from the response body.

### The `Ping` you have to add to the Runtime port

`repository.Runtime` has no `Ping`. Add one — it is the one operation that
distinguishes "the Docker daemon is reachable" from "the daemon is gone and
every deploy is about to fail." The Docker implementation delegates to the
client's ping; the hostexec implementation has nothing to reach, so it always
succeeds.

### Registration details

Health endpoints must not require a session (a probe has no cookie) and must
not go through the CORS wrapper (no browser origin is involved). Register
them outside both.

`/internal/stats` (the worker pool counters) must **not** be public — queue
depth and failure counts are reconnaissance for anyone probing your capacity.
Gate it behind a shared token from config, compared in constant time, and
return **404 rather than 401** so you do not confirm the endpoint exists.

Also exclude `/healthz` and `/readyz` from the request logger, or at a
10-second probe interval your logs become 99% probe noise.

---

## 2.5 Image garbage collection

### The arithmetic

A Next.js image with `node_modules` is 300MB–1.2GB. You removed the
previous-image deletion from `Deploy` — correctly, since it made rollback
structurally impossible — but nothing replaced it.

50 projects × 20 deploys × ~500MB ≈ **500GB**, plus unbounded build cache on
top. When the disk fills, `ImageBuild` fails for *every* project
simultaneously, and the failure surfaces as "no space left on device" in a
build log the user cannot act on.

You need retention, not deletion: **keep the last N images per project.**
N=5 gives a real rollback window at bounded cost. It is a direct
disk-for-recoverability trade, so make the number a named constant.

### Why "delete on deploy" alone is insufficient

Deleting the 6th-oldest image after each successful deploy is necessary but
not sufficient, because images are also orphaned by paths that never reach a
successful deploy: a project deleted while its images exist, a build that
succeeded but whose deploy then failed, and everything from before the GC
existed.

So: **retention at deploy time, plus a periodic sweep** that reconciles every
project against the DB.

### The rules the collector must respect

- **The live deployment's image is untouchable regardless of age.** Removing
  it would break the running container's ability to restart and leave
  Docker's own reference counting as the only thing between you and an
  outage.
- Only reclaim images whose deployment reached a **terminal** state. A row
  still building or running may be about to reference it.
- Deduplicate by image ref before counting — several deployment rows can
  share one image, and counting rows rather than images silently shrinks
  your retention window.
- **Failures are benign.** A removal conflict means a container still holds
  the image; the next sweep gets it. A GC failure must never fail a deploy
  that has already succeeded.

Hook it in at the very end of the pipeline, after the route flip and old
container teardown, in a detached goroutine — the user's deploy must not wait
on disk reclamation. Use `context.WithoutCancel` so it keeps the request ID
and trace values while shedding the cancellation, which is exactly right for
fire-and-forget work started from a request.

### Build cache is separate, and also unbounded

Retention on tagged images does nothing about Docker's build cache. Prune it
periodically with an **age filter** (~one week) — never a bare prune-all,
which would discard the warm cache that makes your incremental builds fast.

### The disk guard in front of all of it

GC is reactive. A pre-flight `statfs` check on the Docker data directory
before starting a build turns "every build fails cryptically" into one clear,
actionable message. If the check itself errors, allow the deploy — do not
block users on your own bug.

Fail fast with a comprehensible message and page yourself, rather than
letting 50 users each discover it via a corrupted build log.

---

## 2.6 Structured logging and request correlation

### What is wrong beyond the profanity

`"job %v fucked up all retries"` and `"WORKER CANCELLEd هی"` obviously have to
go, but the structural problems matter more:

1. **Unparseable.** No machine can extract a project ID or a duration from
   free text, so you cannot build a dashboard, an alert, or a query.
2. **No correlation.** A deploy touches an HTTP handler, a queue worker, the
   pipeline, the builder, the runtime, and the event consumer. Nothing ties
   those lines together. Debugging one user's failed deploy means grepping
   by project ID and hoping it appears in every relevant line — it does not.
3. **No levels.** One undifferentiated stream, so you cannot turn debug up
   during an incident or down in steady state.

### `log/slog` — stdlib, no dependency

JSON in production so it is queryable, text in development so it is readable,
source location on at debug level only. Set it as the slog default too, so
any package still logging directly lands in the same stream.

### Request IDs — the piece that makes logs usable

Middleware that assigns every request an ID (honouring an inbound
`X-Request-ID` if present), echoes it in a response header so a user can
quote it in a bug report, and puts it in the request context.

### The async boundary — the subtle part

The request context dies when the handler returns, but the deploy it
triggered runs for minutes afterwards on a worker. So the request ID must be
**copied into the `Job` struct by value**, not read from a context. That
field is the only thread connecting the user's click to the build log five
minutes later.

The worker then builds a logger with `request_id`, `job_id`, and `project_id`
bound, and stashes it in the job's context. Every layer below does a
one-line `From(ctx)` and inherits those fields for free. The getter must
never return nil — a missing logger is a wiring bug, not a reason to
nil-panic mid-incident.

One query — `request_id="abc123"` — then returns the complete story of one
deploy across six components. That is the whole payoff.

### The access log

Emit fields, not a sentence: request ID, method, path, status, bytes,
duration, IP. Skip health probes. Keep the first-write-wins fix in
`statusRecorder` — that was the bug making the access log report 200 on a
response that actually went out as 500.

### Config additions

`log_level`, `log_format`, and `internal_token` on `ServerConfig`.

### Metrics — the smallest useful version

Do not install Prometheus yet. Four numbers from `GetStats` behind
`/internal/stats`, scraped by anything:

- **`queue_depth`** — the leading indicator. Rising means you are
  capacity-bound.
- **`failed / submitted`** — your deploy success rate.
- **`breaker_open`** — a boolean meaning "something systemic is broken."
- **`rejected`** — users being turned away, invisible everywhere else.

Add build duration as a histogram when you outgrow this. Those four answer
"is it healthy" and "is it fast enough," which is most of the value.

---

## 2.7 Secrets and backups

### The DB password in `configuration.json`

Plaintext, on disk, in a file that shows up in `git status`. The fix is not a
vault yet — it is an **environment override**, so the committed file holds no
secret at all.

Environment wins over the file: the file carries structure and defaults,
secrets come from the process environment, which is not committable, not
exposed by a stray `cat` of the repo, and is what every deployment system
(systemd `EnvironmentFile`, Docker secrets, Kubernetes) already speaks. Do
this for the DB password and host, the internal token, and the env-encryption
key from §3.2.

Then commit the example config with an empty password, and add a **startup
assertion** that an empty password is fatal — so a misconfigured deploy fails
loudly at boot rather than quietly at the first query.

### Backups — the part with no code

You have zero backup story. Postgres holds every user, session, project,
deployment history, and env var. Losing it loses the platform, and the
container images on disk will not reconstruct it.

Minimum viable: `pg_dump --format=custom` on a 6-hour cron (custom format so
`pg_restore` can do selective, parallel restores, with compression built in),
pushed **offsite**, with local copies pruned after ~14 days. A backup on the
same disk as the database is not a backup — it is a second copy of the thing
that is about to fail.

Two non-negotiables people skip:

1. **Test the restore.** An untested backup is a hypothesis. Restore into a
   scratch database monthly and confirm row counts.
2. **Alert on absence.** A backup job that has silently failed for three
   weeks is worse than no backup, because you believed you had one. Have the
   job touch a heartbeat file and alert when it goes stale.

Once you have paying users, turn on WAL archiving for point-in-time recovery
so your worst case is minutes of loss, not six hours.

---
---

# TIER 3 — Security and product gaps

---

## 3.1 Login brute force

### The gap

`NewIPRateLimiter` exists and works — and is applied only to `/upload` and
`/import/github`. `/auth/login` has nothing. With bcrypt cost 12 (~250ms per
attempt), one attacker on one connection gets ~4 attempts/second; with 50
parallel connections, ~200/second. That walks a common-password list against
a known email in hours.

There is a second, quieter problem: **bcrypt cost 12 is itself a DoS vector.**
250ms of pure CPU per attempt means roughly 4 concurrent attackers can
saturate a core. Your login endpoint is the cheapest way to take the entire
control plane down.

### Why you need two limiters

| Limiter | Key | Defends |
|---|---|---|
| Per-IP | client IP | One attacker spraying many accounts, and the CPU-exhaustion angle |
| Per-account | normalized email | A distributed attacker (botnet, proxy pool) hitting one account — every request from a different IP, so the IP limiter never fires |

Neither alone is sufficient. Per-IP misses distributed attacks; per-account
misses spraying and CPU exhaustion.

Design details that matter:

- **Count failures, not attempts** — a user with the right password is never
  locked out by their own successful logins.
- **Check before the password is verified.** A blocked request must never
  reach bcrypt; that is what closes the CPU-exhaustion vector.
- **Sliding reset:** each further failure pushes the window out, so a
  persistent attacker stays locked out rather than getting a fresh budget
  every window.
- **A success clears the account counter but NOT the IP counter.** One
  correct password from a shared NAT gateway must not reset the budget an
  attacker behind the same gateway is burning.

Suggested numbers: 20 per IP, 5 per account, 15-minute window.

Because the limiter needs the email, it cannot be a plain wrapping
middleware — it goes inside the login handler after the body is decoded.
Note that `normalizeEmail` is unexported in `application`; the limiter key
must match the lookup key, or `bob@x.com` and `BOB@x.com` get separate
budgets.

### The timing side channel

Your login is enumeration-safe in its *message* but almost certainly not in
its *timing*. If the user is not found you return before calling bcrypt, so
"unknown email" answers in ~1ms and "wrong password" in ~250ms. That is a
trivially measurable oracle for whether an address is registered — it defeats
the entire point of the identical error message.

Fix: on the not-found path, compare the supplied password against a fixed
dummy hash computed once at package level. Both paths then cost the same wall
clock time.

---

## 3.2 Env var encryption at rest

### The threat model — be precise about what this buys

Env vars are the highest-value data you hold: users will put Stripe keys,
database URLs, and API tokens there. Today they are plaintext columns.

Encryption with a key in the process environment defends against exactly one
thing, but it is the most likely thing:

- ✅ **A leaked database** — a stolen backup, a misconfigured replica, SQL
  injection, a laptop with a `pg_dump` on it. Ciphertext is useless without
  the key, which lives in the process environment, not in Postgres.
- ❌ **A compromised control-plane host** — the key is in that process's
  memory and environment. Defending that needs a KMS or HSM, which is a
  later problem.

That asymmetry is the whole argument: database compromise is far more
probable than host compromise, and this is a few hours of work.

### AES-256-GCM, and why not CBC or plain CTR

GCM is **AEAD** — authenticated encryption. It produces a tag that detects
any modification of the ciphertext. Without authentication, an attacker with
write access to the DB can flip bits and change the decrypted plaintext in
controlled ways. For a value that becomes an environment variable inside a
container you execute, that is a code-execution primitive. Never use
unauthenticated encryption for anything you will later act on.

Two implementation rules:

- **A fresh random nonce per encryption, from `crypto/rand`.** Reusing a
  nonce with the same key in GCM leaks the XOR of the two plaintexts *and*
  allows forging the authentication tag. It is a total break, not a
  weakening. The nonce is not secret, so prepend it to the ciphertext.
- **Never fall back to the raw column value on a decrypt failure.** A failed
  decrypt means a wrong key or tampering; handing the ciphertext to a
  container as an env var would be worse than failing the deploy.

### Where it goes

In the Postgres `EnvVarRepository` implementation, **not** the use case. Then
nothing above the persistence layer knows ciphertext exists — use cases, the
pipeline, and handlers keep dealing in plaintext, and there is exactly one
place a bug could leak a raw value into a column.

### Migrating existing plaintext rows

Add an explicit version prefix (`enc:v1:`) to ciphertext, and have decrypt
pass through anything without it. Rows written before encryption keep working
and get upgraded on their next write. Remove the passthrough branch once a
backfill confirms no unprefixed rows remain.

The version tag is also what makes key rotation possible later without
guessing which scheme produced a row.

### Do not log them

Audit for `%+v` on anything holding an `EnvVar` — that will happily put a
Stripe key in your logs. Give the entity a `String()` that redacts the value;
both `fmt` and `slog` honour `Stringer`, so one method covers most accidental
paths.

---

## 3.3 TLS, `secure_cookies`, and the wildcard certificate

### The flags that must flip together

`Secure` means the browser will not send the cookie over plain HTTP. Flip it
before TLS exists and every login silently fails to persist; ship TLS without
it and every session cookie is sniffable on any shared network. One deploy,
both changes.

Confirm the session cookie has all four attributes: `HttpOnly` (JS cannot
read it, so XSS cannot steal the session), `Secure`, `SameSite=Lax` (CSRF
defence — Lax rather than Strict so a link from an email still lands the user
logged in), and a real expiry.

### The wildcard certificate — the part that is actually hard

Every tenant gets `{slug}.yourdomain.com`. Per-host on-demand TLS means a
Let's Encrypt issuance per project, which hits the rate limit (50
certificates per registered domain per week) the moment you have real
signups. You need **one wildcard cert for `*.yourdomain.com`**.

Wildcards require the **DNS-01 challenge** — HTTP-01 cannot validate one.
That means Caddy needs API credentials for your DNS provider, and a Caddy
binary built with that provider's plugin, because the stock binary has none
(built via `xcaddy`).

Two details people get wrong:

1. **A wildcard covers exactly one label.** `*.yourdomain.com` matches
   `app.yourdomain.com` but **not** `a.b.yourdomain.com`. If a slug can ever
   contain a dot, routing breaks. Verify `slugify` strips dots.
2. **Scope the DNS token** to zone-DNS-edit on that one zone. A global API
   key on the box that runs untrusted user containers is a very bad trade.

### Reserved subdomains

Nothing currently stops a user claiming `api`, `www`, `admin`, `app`, `docs`,
or `mail`. `api.yourdomain.com` in a tenant's hands is a credible phishing
and cookie-scoping problem. Add a denylist enforced in the same place
uniqueness is — reject or auto-suffix, consistent with your existing slug
behaviour.

### Host header trust

With Caddy terminating TLS, your Go process sees whatever Caddy forwards.
Ensure Caddy sets `X-Forwarded-For` and `X-Forwarded-Proto`, and **bind the
control-plane listener to `127.0.0.1`** so it is not reachable directly from
the internet. Otherwise a direct connection can spoof `X-Forwarded-For` and
defeat every IP-based limiter you just added.

---

## 3.4 Email verification and password reset

Both need the same primitive — a single-use, expiring, hashed token — which
you already built for sessions. Reuse the shape.

### The token table

A `user_tokens` table with: id, user_id (cascade delete), **purpose**
(constrained to `email_verify` / `password_reset`), token_hash (unique),
expires_at, used_at, created_at. Index on the hash and on
(user_id, purpose).

**Purpose is not optional.** It keeps a password-reset token from being
redeemed as an email verification and vice versa. Never make a token
type-agnostic.

Store **SHA-256 of the token, never the token** — a leaked database must not
hand over working reset links, same reasoning as sessions. SHA-256 rather
than bcrypt is deliberate and correct here: these are 32 bytes of
`crypto/rand`, so there is no dictionary to attack and no need for a slow
hash. Bcrypt's cost only earns its keep against low-entropy human passwords.

### Password reset — the four rules

1. **Always return success**, even for an unknown email. Reporting "no such
   user" turns this endpoint into an account-enumeration oracle — exactly
   what your login error message is careful to avoid.
2. **Invalidate outstanding tokens first**, so a stolen older link stops
   working the moment a new one is requested.
3. **32 bytes from `crypto/rand`.** Never `math/rand`, never a UUID, never
   anything derived from user data or the clock.
4. **Short TTL** — one hour. A reset link is a live credential sitting in an
   inbox.

### The redemption step everyone forgets

After updating the password hash and marking the token used: **revoke every
existing session for that user.** The whole point of a reset is often that
someone else has the account — if their session survives it, the reset
accomplished nothing.

That single call is the concrete payoff of choosing sessions over JWTs.
With stateless JWTs you would need a separate revocation list, which is a
session store wearing a disguise.

### Email verification and the gating decision

Same mechanism, `email_verify` purpose, 24-hour TTL. The design question is
what unverified users may do. Recommended: they can register, log in, and see
the dashboard — but **cannot deploy.**

Gate deploys, not login. Deploying is the expensive, abusable action, so
requiring verification there stops throwaway-address abuse without adding
friction to signup. Enforce it next to `enforceProjectLimit`.

### Sending mail

Do not run your own SMTP. Use a provider (Resend, Postmark, SES) behind a
small `Mailer` port so the use cases do not depend on it, and ship a
log-to-stdout implementation for local development so nothing is blocked on
provider signup.

---
---

# Landing order

Dependencies here are real, and two items **must ship together**.

### Batch 1 — same PR, non-negotiable (~1 day)

§1.2 stats atomics · §1.3 pool rewrite · §2.1 graceful shutdown

Why together: adding graceful shutdown *arms* the `close(wp.jobs)` panic that
is currently dormant only because `ShutDown` never runs. Landing §2.1 without
§1.3 makes the system strictly worse than it is today.

### Batch 2 — the silent data-loss bug (~half a day)

§1.1 Caddy `@id` routing · §2.2 abandoned-deployment sweeper

§1.1 needs the one-time route flush at deploy time. §2.2 cleans up every
zombie row those restarts have already created.

### Batch 3 — operability (~1 day)

§2.4 health endpoints · §2.3 timeouts + upload limit · §2.6 structured
logging

Logging last of the three because it touches the most files and you want the
earlier changes stable first.

### Batch 4 — before real users (~1 day)

§3.1 login limiting + timing fix · §2.5 image GC + disk guard · §3.3 TLS,
secure cookies, wildcard cert, reserved slugs · §2.7 secrets and backups

### Batch 5 — before paying users

§3.2 env var encryption · §3.4 email verification and password reset

### What NOT to do yet

- **Prometheus/Grafana** — four counters cover you to ~100 users
- **Distributed tracing** — you have one process
- **Multi-node orchestration** — the `Runtime` seam already makes this a
  later, additive change, which is exactly why it can wait
- **Kubernetes** — it would replace `dockerrun`, and `dockerrun` is one of
  the better-built parts of the codebase

### Tests worth adding alongside

- `go test -race ./internal/queue/...` — proves §1.2 and §1.3
- A Caddy client test against `httptest.Server`: assert the `PUT /id/...`
  path, then the 404 fallthrough to insert
- A sweeper test: a `building` row in, a `failed` row out
- **A concurrent-deploy test** — two projects, simultaneous route flip,
  assert both routes survive. This is the regression test for §1.1 and the
  most valuable single test on the list.
