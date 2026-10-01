Let me get the facts before answering — especially for the folder question, which has a hard Go constraint.

Facts gathered. Three answers.

---

# 1. What's left for a production deployer

Sorted by "will this hurt me," not by effort.

## Tier 1 — bugs that bite *now*, at 4 workers, not at scale

**The Caddy route race.** `RegisterRoute` does GET-whole-array → mutate → PATCH-whole-array with no lock. Two deploys finishing simultaneously: the second PATCH overwrites the first's route. That project built fine, started fine, said "live at…", and 502s forever with no error logged anywhere. You have 4 workers and every deploy ends in a route flip — this is reachable with a handful of users.

**A real data race in the worker pool.** `wp.stats.{submitted,completed,failed}` are plain ints incremented from 4 concurrent goroutines with no mutex. `go test -race` would fail on this today.

**`ShutDown()` can panic.** It calls `close(wp.jobs)`, but `EventConsumer.maybeRestart` submits from a `time.AfterFunc` goroutine — send on a closed channel is a panic, not an error. Currently masked only because `ListenAndServe` blocks forever and shutdown never actually runs.

**Retry amplification.** `Process` retries 3× with `time.Sleep` *inside the worker* (so a worker sits idle instead of working), and `EventConsumer` independently restarts crashed deploys up to 3× more. One systemic failure (disk full, daemon wedged) becomes ~12 attempts per project, peaking load exactly when the system is least able to take it.

## Tier 2 — you cannot operate it without these

**No graceful shutdown.** No `signal.Notify`, no `server.Shutdown()`. Deploy on `SIGTERM` = build killed mid-flight, container orphaned, deployment row stuck in `building` **forever** — and nothing ever cleans that up. `Reconciler` only handles "DB says running, runtime doesn't have it," not "DB says building, nobody is building it." Every restart during a deploy leaks a zombie row.

**No `/healthz`.** Nothing can supervise, restart, or load-balance this.

**No HTTP server timeouts.** `ReadTimeout`/`IdleTimeout`/`ReadHeaderTimeout` all unset — slowloris and leaked connections. (Careful: a blanket `WriteTimeout` would kill your SSE streams — `ReadHeaderTimeout` + `IdleTimeout` are the safe two.)

**No image GC.** Still nothing. Disk grows monotonically, forever, by ~a few hundred MB per deploy.

**Logging.** `log.Printf` with profanity in it (`"job %v fucked up all retries"`, `"WORKER CANCELLEd هی"`). Beyond the obvious, there's no structure, no request ID carried into the pipeline, no metrics. You cannot answer "why was that deploy slow" or "what's our failure rate."

**Secrets on disk.** DB password in plaintext `configuration.json`. No Postgres backup story at all.

## Tier 3 — product/security gaps

Rate limiting covers `/upload` and `/import/github` but **not `/auth/login`** — brute force is wide open. Env vars are plaintext by deliberate MVP choice. No email verification or password reset. `secure_cookies: false` must flip once TLS + wildcard certs exist.

---

# 2. Brutally honest rating

## What's genuinely good — and I mean genuinely, not politely

**The interface seams are in the right places.** `repository.Runtime` and `repository.ImageBuilder` are drawn exactly where they should be. The proof isn't theoretical: swapping host-exec → Docker touched zero business logic, and the eventual Kubernetes path is "write an implementation," not "rewrite the pipeline." Most people get this wrong by abstracting the wrong axis. You didn't.

**The deploy ordering is properly reasoned.** Build → start → wait-ready → flip route → *then* tear down the old one. Zero-downtime and fail-safe in the right direction. A lot of shipped, funded products get this backwards and take the site down on every failed deploy.

**Container hardening is above average for this class of project.** cap-drop ALL, no-new-privileges, read-only rootfs, pids cgroup, swap disabled, no host mounts, no published ports. The typical hobby PaaS is `docker run` with defaults and a prayer.

**Ownership enforcement is disciplined.** One `mustOwnProject`, applied uniformly, 404-not-403 everywhere. That consistency is rarer than it should be.

## What's genuinely weak

**The queue is the weakest thing in the codebase by a wide margin, and it's load-bearing.** In-memory channel, data race on stats, panic on shutdown, retry storm, `time.Sleep` blocking workers, and zero durability — every queued and in-flight job evaporates on restart with no record. Everything else in this system looks *designed*. This looks written once and never revisited. It's the single biggest liability you have.

**No durability of in-flight work, generally.** Not just the queue — the whole pipeline assumes the process survives. Nothing recovers a half-finished deploy.

**Error handling by string matching.** `isNotFound` / `isConflict` / `isInvalidRollback` doing `strings.Contains` on error text. It works, but rewording an error message silently changes an HTTP status code. Typed sentinel errors are the standard fix and you'll want them before this gets bigger.

**Observability is effectively zero.** Covered above, but it belongs on the architecture critique too: you can't operate what you can't see, and right now there's nothing to see.

**Cosmetic inconsistency that signals rush:** `NewUplaodHandler`, the `dockerfilee.go` typo, `ProjectDeployStatusAlias` (a type alias that exists to make autocomplete nicer), handlers split between using the passed `ctx` and `r.Context()`.

## The number

| | |
|---|---|
| Architecture & layering | **8/10** — real, not cargo-culted. The seams are correct. |
| Implementation quality | **6/10** — careful in places, prototype-grade in others, unevenly. |
| Production readiness | **4/10** — live correctness bugs, no shutdown/health/observability/durability. |
| Security posture | **7/10** — containers well above average; auth fresh and unhardened. |

**Overall, for a solo-built MVP PaaS: genuinely above average, and better-architected than most things at this stage.** The honest framing is that "well-architected" and "production-ready" are two different axes, and you're strong on one and weak on the other. The gap between them is Part 1's list — and importantly, that list is all *additive* work. None of it requires undoing a design decision. That's the good position to be in; the expensive mistake would have been getting the `Runtime` abstraction wrong, and you didn't.

---

# 3. The folder question

**Hard constraint first: Go packages cannot span directories.** One directory = exactly one package. There's no build tag, no directive, no escape hatch. `application/auth/*.go` being `package application` is impossible. So the thing you're asking for specifically can't be done.

Your real options:

## Option A — file-name prefixes (what I'd do)

Zero risk, zero import changes, no code moves. Editors and `ls` sort them into visual clusters:

```
auth_register.go      auth_login.go       auth_logout.go
auth_password.go      auth_session.go

project_upload.go     project_import.go   project_claim.go
project_get.go        project_list.go     project_delete.go
project_rename.go     project_env.go      project_limit.go
project_slug.go       project_dto.go      project_ownership.go

deploy_pipeline.go    deploy_run.go       deploy_stop.go
deploy_rollback.go    deploy_reconcile.go deploy_events.go
deploy_logs.go        deploy_env.go       deployment_dto.go

housekeeping_reaper.go
```

Pure `git mv`, nothing else changes. 2,898 lines across 33 files honestly isn't large for Go — your `infrastructure` tree is 6,575.

## Option B — actually split into sub-packages

I checked what's shared, and this is the real cost. These unexported symbols cross the whole package:

`mustOwnProject` (~8 use cases) · `stopTimeoutSeconds` (5 files) · `slugify` / `uniqueSlug` / `randomSuffix` · `appEnv` · `enforceProjectLimit` · `newProjectSummary` / `newDeploymentSummary` / `newDeploymentStatusInfo`

A naive split breaks all of them. You'd need a shared package underneath and you'd be fighting import cycles.

**One clean cut does exist though:** `auth` (register/login/logout/password/session_token) shares exactly *one* helper with the rest — `normalizeEmail` — and touches none of the project/deploy machinery. It's a genuine bounded context and would extract cleanly into `internal/application/auth` today. Everything else is legitimately interconnected and would just get worse if forced apart.

**My recommendation:** Option A now, and consider extracting `auth` only if the package keeps growing. Don't split projects/deploy — the coupling there is real domain coupling, not accidental mess, and hiding it behind package boundaries would make it harder to see, not easier.

---

Want me to do the `git mv` renames for Option A, or start knocking out Tier 1? The Caddy mutex and the stats race are both about ten minutes each.






---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---
---


