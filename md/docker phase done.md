Now I'll build the package.

Now the runtime core, logs and events:

Phase 3 is in. `go build ./...` clean, `go test ./internal/...` green, `gofmt` clean.

---

# What got built

`internal/infrastructure/dockerrun/` — 671 lines of implementation, 196 of tests. Nine files, split so that **everything pure is separated from everything that touches the daemon**. That's the single organizing principle: the security policy, the label mapping, and the state mapping are pure functions with no `*client.Client` anywhere near them, which is why the whole hardening surface is unit-tested without Docker running.

```
dockerrun/
├── runtime.go   ← the interface impl. Start/Stop/Remove/Status/List
├── logs.go      ← Logs
├── events.go    ← Events
├── spec.go      ← pure: RuntimeSpec → Docker create payload  (Phase 4 lives here)
├── labels.go    ← pure: label namespacing + filter construction
├── status.go    ← pure: Docker state → entities.RuntimeState
├── config.go    ← pure: deployment-wide policy + defaults
├── network.go   ← EnsureNetwork, called once at boot
└── dockerrun_test.go
```

---

## The type

```go
type DockerRuntime struct {
	cli *client.Client
	cfg Config
}

var _ repository.Runtime = (*DockerRuntime)(nil)
```

Two fields, both injected. No mutex, no map, no goroutine registry — **that's the whole point of the migration.** `HostExecRuntime` needed `mu sync.RWMutex` + `processes map[string]*TrackedProcess` because the process table only existed in this program's memory. Here the daemon *is* the registry, so `DockerRuntime` is stateless and every method is a translation.

The `var _ repository.Runtime` line is a compile-time assertion — if the interface changes, this file fails to build rather than `main.go` failing at the wiring site with an unreadable error.

---

## `runtime.go` — the five synchronous methods

### `Start` — [runtime.go:41](internal/infrastructure/dockerrun/runtime.go#L41)

```
translate(spec) → ContainerCreate → (on 409: force-remove, retry once) → ContainerStart → return ID
```

Four things worth knowing:

**The handle is the container ID.** Full 64-hex, opaque, stored by the pipeline into `deployments.container_id` via `SetContainerInfo`. Every other method takes it straight back as `string(handle)`.

**`ctx` bounds the API calls only — not the container.** `HostExecRuntime.Start` has a fifteen-line comment explaining why it must *not* use `exec.CommandContext`: the worker pool cancels the deploy context the instant `Deploy()` returns, which would SIGKILL the app milliseconds after a successful deploy. That entire hazard is gone. The daemon owns the container's lifetime, so passing the deploy context here is correct and harmless.

**Name-conflict recovery.** The container is named `golaunch-<deploymentID>`. If a stale container from a previous run already holds that name, `ContainerCreate` returns 409. Rather than failing the deploy, it force-removes and retries once. The name encodes a deployment ID that is being deployed *right now*, so anything already holding it is by definition stale. This is what makes deploys idempotent across a control-plane crash.

**Cleanup on start failure.** If `ContainerStart` fails, the created-but-never-started container is force-removed. Otherwise it sits there holding the name and every subsequent retry hits the conflict path.

### `Stop` — [runtime.go:76](internal/infrastructure/dockerrun/runtime.go#L76)

`ContainerStop` with `StopOptions{Timeout: &timeout}`. The daemon sends SIGTERM, waits `timeout` seconds, then SIGKILL — which maps **exactly** onto the interface's `timeoutSeconds` parameter and onto the 30 lines of `syscall.Kill(-pgid, SIGTERM)` / `time.After` / `syscall.Kill(-pgid, SIGKILL)` in the hostexec version. Not-found is success: the caller wanted it stopped, and it is.

The signal actually reaches the app because [dockerfile.go](internal/infrastructure/dockerbuild/dockerfile.go) emits `CMD` in exec form. Shell form would put `/bin/sh` at PID 1, which doesn't forward SIGTERM, and every stop would be a hard kill with no graceful drain.

### `Remove` — [runtime.go:87](internal/infrastructure/dockerrun/runtime.go#L87)

Deliberately **not** forced. Docker returns 409 for a running container; that's translated into `cannot remove running container %s, stop it first` — matching hostexec's semantics exactly, so `Reconciler` and `DeployPipeline` behave identically under either runtime. `RemoveVolumes: true` so anonymous volumes don't accumulate.

The private `remove(ctx, ref, force)` helper is shared with `Start`'s two cleanup paths, which *do* force.

### `Status` — [runtime.go:105](internal/infrastructure/dockerrun/runtime.go#L105)

`ContainerInspect` → `statusFromState`. Not-found maps to `RuntimeStateNotFound`, not an error — same contract as hostexec's missing-map-key path.

### `List` — [runtime.go:127](internal/infrastructure/dockerrun/runtime.go#L127)

`ContainerList{All: true, Filters: labelFilters(labelFilter)}`.

**This is the method with real teeth.** `Reconciler.Run` calls `r.Runtime.List(ctx, nil)` and then **stops and removes everything it doesn't recognize** ([reconcile.go:83-91](internal/application/reconcile.go#L83-L91)). Under hostexec, "everything" meant this program's own child processes — bounded and safe. Under Docker, an unfiltered list is *every container on the host*: your Caddy, your Postgres, anything else you're running. The reconciler would have torn the box down on first boot.

So `labelFilters` injects `golaunch.managed=true` **unconditionally**, at the bottom of the stack where no caller can forget it. A nil filter means "all of ours," never "all on the host." There's a test asserting exactly this.

`Summary` carries no exit code or OOM flag, so `statusFromSummary` maps running/not-running only. That's what `List` is for — cheaply answering *what is alive* — and it's what the reconciler actually consumes.

---

## `spec.go` — where Phase 4 actually lives

`translate(spec)` returns `(*container.Config, *container.HostConfig, *network.NetworkingConfig)`. It makes zero daemon calls, which is why the security policy is fully covered by `TestTranslateAppliesHardening` with no Docker available.

**Container naming** — `golaunch-<deploymentID>`, and I want to flag one decision:

> Naming from the **deployment ID, not the slug**, and attaching **no network alias**. During a route flip both the old and new deployment are alive on the same network for a moment. A shared name or a shared `slug` alias would make Docker's embedded DNS round-robin between the two versions — traffic silently splitting across old and new for the length of the flip. Deployment-scoped names make Caddy's route swap the single atomic switch it's supposed to be.

**The `HostConfig`, item by item:**

| Setting | Why |
|---|---|
| `CapDrop: ["ALL"]`, no `CapAdd` | Node needs zero capabilities to bind :3000 |
| `SecurityOpt: no-new-privileges:true` | setuid binaries can't escalate |
| `ReadonlyRootfs: true` | a compromised app can't persist anything |
| `Tmpfs` | `/tmp` and `/app/.next/cache`, both `noexec,nosuid`, size-capped — the writable holes a read-only rootfs needs |
| no `Binds`, no `Mounts` | zero host filesystem visibility |
| no `PortBindings`, `PublishAllPorts: false` | **not addressable from the host at all** |
| `NetworkMode: golaunch-edge` | joined only to the edge network, ICC off |
| `Memory` + `MemorySwap` equal | swap disabled → prompt OOM kill instead of host thrash |
| `NanoCPUs` | CPU ceiling |
| `PidsLimit` | fork-bomb containment |
| `Ulimits` nofile/nproc | fd and process ceilings |
| `Init: true` | tini as PID 1 — Node doesn't reap orphans |
| `RestartPolicy: "no"` | see below |

`RestartPolicy: no` is deliberate and worth defending: `EventConsumer` owns restart decisions and caps crash counts. If the daemon also auto-restarted, the two would fight and a crash loop would be invisible — the container would keep coming back while the crash counter never advanced.

One thing I decided against: read-only rootfs is applied **unconditionally** as a platform guarantee, so `entities.ResourceLimits.ReadOnlyFS` is not consulted. It's noted in the file. Memory/CPU/pids *are* per-project and flow through `Config.resolveLimits`, which overlays spec values on deployment defaults so a spec with zero limits still lands inside a cgroup — also tested.

**`ContainerAddress(deploymentID, port)`** is exported for Phase 5: the pipeline needs the Caddy upstream before it has anything back from the runtime except the deployment ID. It's the replacement for `fmt.Sprintf("localhost:%d", port)`.

---

## `labels.go` — the namespace boundary

Docker labels are one flat namespace shared with every container on the host. The application layer speaks bare keys (`deployment_id`, `slug`) — that's baked into [reconcile.go:56](internal/application/reconcile.go#L56) and [event_consumer.go:94](internal/application/event_consumer.go#L94). Bare keys on the daemon side would be a collision waiting to happen.

Three symmetric functions:

- `encodeLabels` — prefixes with `golaunch.` and stamps `golaunch.managed=true`
- `decodeLabels` — strips back. Also serves event actor attributes, where container labels arrive **mixed in with daemon keys** like `name`, `image`, `exitCode`; the prefix check drops those for free
- `labelFilters` — builds `filters.Args`, always managed-scoped

The application layer never sees a prefix. Swap the runtime and nothing above the wiring notices.

---

## `logs.go` — demultiplexing

A non-TTY container's log stream is **one connection with 8-byte frame headers** carrying interleaved stdout and stderr. Containers are always created with `Tty` false, so the framing is always there.

```
ContainerLogs → io.Pipe pair ← stdcopy.StdCopy → 2× bufio.Scanner → chan LogLine
```

`stdcopy.StdCopy` unpacks frames into two `io.Pipe` writers; two scanner goroutines turn each side into `LogLine{Stream: stdout|stderr}`; a `sync.WaitGroup` closes the channel once both drain.

**The cancellation detail:** a follow stream blocks inside `Read` until the container writes, so `ctx.Done()` can't interrupt it. A watchdog goroutine closes `body` on cancel, which unblocks the read. That watchdog is torn down via a local `done` channel closed by defer — otherwise a non-follow read with `context.Background()` would leak a goroutine parked on `ctx.Done()` forever.

Scanner buffer capped at 1MB so one pathological log line can't grow unbounded.

**What this buys you:** hostexec kept logs in a per-process in-memory ring buffer — gone on restart. These come from the daemon's log driver, so history survives a control-plane restart and `Tail` can reach back past this process's own lifetime.

---

## `events.go` — the reconnecting pump

`Events(ctx)` starts a goroutine and returns a fresh channel per call. Note this differs from hostexec, which hands every caller the *same* channel — meaning two consumers would steal each other's events. Per-call streams are strictly better and the interface allows it.

```
pumpEvents  ← outer loop: reconnect w/ 2s backoff, carries the `since` bookmark
  └─ streamEvents  ← drains one subscription, returns the bookmark
       └─ translateEvent  ← pure: events.Message → entities.RuntimeEvent
```

**The bookmark is the important part.** `Since` is set to the last message's `TimeNano + 1`. If the daemon restarts or the connection drops, the resumed stream replays events from the gap — without it, a `die` event lost mid-reconnect leaves a project marked `running` in the DB **forever**, with nothing to ever correct it. The `+1` prevents replaying the last event twice.

Filters are daemon-side: `type=container`, `label=golaunch.managed=true`, `event=start|die|oom`.

`translateEvent` maps action → `RuntimeEventType`, `Actor.ID` → handle, `decodeLabels(Actor.Attributes)` → labels. That last one is how `EventConsumer` resolves an event to a deployment with no DB lookup.

**`RuntimeEventOOM` is now real.** It's been declared in [entities/runtime.go](internal/domain/entities/runtime.go) since the start and never once emitted, because nothing in hostexec could observe memory pressure. The daemon emits `oom` immediately before `die`, so OOM deaths are now distinguishable from ordinary crashes.

---

## `network.go` — boot-time prerequisite

`EnsureNetwork(ctx, cli, name)` — inspect, create if absent, treat a concurrent creator's 409 as success. Driver `bridge`, with:

```go
"com.docker.network.bridge.enable_icc": "false"
```

ICC off means containers on the network **cannot reach each other**. Only Caddy, which initiates the connection, can reach them. A single compromised tenant app is a dead end instead of a foothold on every other app on the box.

This must run before any deploy — a create against a missing network just fails.

---

## Where it wires up (not yet done — that's Phase 6)

Everything above the wiring is untouched. `main.go` today:

```go
hostRuntime := hostexec.NewHostExecRuntime()
```

becomes, once Phases 5 and 6 land:

```go
cli, err := dockerx.NewClient(ctx)          // already exists
dockerrun.EnsureNetwork(ctx, cli, dockerrun.DefaultNetwork)
runtime := dockerrun.New(cli, dockerrun.Config{})
builder := dockerbuild.NewDockerImageBuilder(cli)
```

`hostRuntime` currently flows into four places — `NewDeployPipeline`, `NewEventConsumer`, `NewReconciler`, `InitializeRoutes` — and all four take `repository.Runtime`, so all four are satisfied by the new value with no edits.

**I deliberately did not do that wiring**, and here's why: switching the runtime without Phase 5 gives you a broken deploy. Three things would be wrong on the first request:

1. [deploy_pipeline.go:163](internal/application/deploy_pipeline.go#L163) registers `localhost:<port>` — but the container publishes no host port. Every route would 502. It needs `dockerrun.ContainerAddress(deployment.ID, port)`.
2. [reconcile.go:79](internal/application/reconcile.go#L79) rebuilds the same `localhost:%s` upstream from the `port` label.
3. [deploy_pipeline.go:146](internal/application/deploy_pipeline.go#L146) passes `append(os.Environ(), ...)` into the container — handing every deployed app your Postgres credentials.

So the current state is: Phase 3 complete, compiled, tested, and *correctly* unwired. `dockerbuild` sits in the same position.

---

## Two things I couldn't do

**No end-to-end run.** Phase 0 host setup isn't done — no daemon socket access, no `golaunch-edge` network. Everything pure is covered by tests; every daemon-touching path is compile-verified only. I'd want one real deploy through it before flipping the default.

**`nproc` is not what it looks like.** The `nproc` ulimit is per-UID on the host, not per-container. If two containers run as the same remapped UID they share the budget. `PidsLimit` is the real per-container guarantee; the ulimit is a second layer. Worth knowing before you tune it.

---

Next is Phase 5 — the pipeline edits, including that env leak. Say go and I'll take it.