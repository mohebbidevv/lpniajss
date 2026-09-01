Good call. Here's the plan — no code written yet.

---

# Docker Integration Plan

**Decision locked:** Caddy runs as a container on a shared Docker network, dialing app containers by name. No app container ever publishes a host port.

## Phase 0 — Host prerequisites (setup, not code)

These are things you run once; nothing in the repo changes.

1. **Daemon access for the control plane.** The server needs to talk to `/var/run/docker.sock`. Two options:
   - `sudo usermod -aG docker $USER` — simple, but docker-group membership is effectively root on the host. Fine for your dev box.
   - **Rootless Docker** — daemon runs as your user, a container breakout lands in an unprivileged user namespace instead of root. This is the right answer for production and I'd set it up now so you're not retrofitting later.
2. **Create the edge network:** `docker network create golaunch-edge` with `com.docker.network.bridge.enable_icc=false` so app containers can't talk to *each other* — only Caddy can reach them.
3. **Run Caddy as a container** on that network, with its admin API bound so the control plane can reach it but nothing else can.
4. **Base image pre-pull:** `node:22-alpine` (or Debian slim if native modules matter), so first deploys aren't waiting on a base-image pull.

## Phase 1 — Extract framework detection out of `hostexec`

`GetProjectSpecs` in [hostexec/detect.go](internal/infrastructure/hostexec/detect.go) is genuinely runtime-agnostic — it reads `package.json` and decides install/build/start commands. The Docker builder needs the exact same logic to generate a Dockerfile.

- Move it to a new `internal/infrastructure/nodedetect/` package.
- `hostexec` imports it instead of owning it. No behavior change.
- Extend it slightly: detect package manager (npm/pnpm/yarn via lockfile) and Node version (`engines.node` or `.nvmrc`), both of which matter for a correct Dockerfile but are irrelevant to host-exec.

## Phase 2 — The builder: `internal/infrastructure/dockerbuild/`

Implements `repository.ImageBuilder` ([imagebuilder_repo.go](internal/domain/repository/imagebuilder_repo.go)).

**`Build()` flow:**
1. **If the repo has its own `Dockerfile`, use it.** Real devs expect this and it's a one-line check. Otherwise generate one.
2. **Generate a multi-stage Dockerfile** from `nodedetect` output:
   - Stage 1 (deps): copy lockfile only → `npm ci` → this layer caches across deploys as long as dependencies don't change, which is the single biggest build-speed win.
   - Stage 2 (build): copy source → run build command.
   - Stage 3 (runtime): slim base, copy only build output + production `node_modules`, `USER node` (never root), `EXPOSE 3000`, start command.
3. **Tar the build context** and stream it to the daemon, honoring `.dockerignore` (and injecting a default one — `node_modules`, `.git` — if absent).
4. **Build via BuildKit** (`DOCKER_BUILDKIT=1`) — better caching, and build steps run in more isolated containers than the legacy builder.
5. **Stream build output to `logSink`** so it still reaches the SSE stream exactly as it does today. BuildKit emits structured JSON progress; needs a decoder that flattens it to `LogLine`s.
6. **Tag as `golaunch/<slug>:<deploymentID>`** — image per deployment is what makes rollback possible later.
7. **Enforce `BuildRequest.Timeout` and `Limits`** — both fields already exist on the struct and are currently ignored. A hung `npm install` must not occupy a build worker forever.

`ImageExists` → `ImageInspect`. `RemoveImage` → `ImageRemove`.

**Note on build-time risk:** `npm install` executes arbitrary `postinstall` scripts. BuildKit sandboxes these in a container, which is already a massive improvement over today (where they run as your user on the host), but builds do need network access for the registry. Constrain with build-time memory/CPU caps and a timeout.

## Phase 3 — The runtime: `internal/infrastructure/dockerrun/`

Implements `repository.Runtime` ([runtime_repo.go](internal/domain/repository/runtime_repo.go)). Method-by-method:

| Interface method | Docker SDK |
|---|---|
| `Start` | `ContainerCreate` + `ContainerStart` |
| `Stop` | `ContainerStop` (sends SIGTERM, then SIGKILL after timeout — maps exactly onto the existing `timeoutSeconds` param) |
| `Remove` | `ContainerRemove` |
| `Status` | `ContainerInspect` → map `State.Running`/`ExitCode`/`OOMKilled` |
| `Logs` | `ContainerLogs` + `stdcopy.StdCopy` to demux stdout/stderr |
| `List` | `ContainerList` with label filters (labels already set by the pipeline) |
| `Events` | `client.Events()` filtered to `die`/`oom` → same `RuntimeEvent` channel shape |

**Three things this makes strictly better than hostexec:**
- `RuntimeEventOOM` becomes real. Today it's declared but never emitted because nothing watches memory.
- **Containers survive a control-plane restart.** This fixes the shaky premise I flagged in `Reconciler` — `List()` will return genuinely-still-running apps, so reconciliation becomes meaningful instead of "everything looks missing, redeploy all."
- Logs come from the daemon, not an in-memory ring buffer, so log history survives a server restart too.

## Phase 4 — Security configuration (the part you actually asked for)

Every container gets all of this. This is the concrete answer to "most security conf":

**Privilege**
- `CapDrop: ["ALL"]`, no `CapAdd`. Node needs zero capabilities when binding a port above 1024.
- `SecurityOpt: ["no-new-privileges:true"]` — blocks setuid binaries from escalating.
- `USER node` in the Dockerfile — never PID 1 as root inside the container.
- User-namespace remapping at the daemon level, so container-root maps to an unprivileged host UID (defense in depth with the above).
- Default seccomp + AppArmor profiles stay **on** (explicitly not `unconfined`).
- Never `Privileged`, never mount the docker socket.

**Filesystem**
- `ReadonlyRootfs: true`.
- `Tmpfs` mounts for `/tmp` and any framework cache dir (`.next/cache`), each size-capped, mounted `noexec,nosuid`.
- No bind mounts from the host into app containers, at all.

**Resources** (fills in `entities.ResourceLimits`, which exists today but is enforced nowhere)
- `Memory` + `MemorySwap` set equal → swap disabled, so a leaking app gets OOM-killed cleanly instead of thrashing the host.
- `NanoCPUs` for a CPU ceiling.
- `PidsLimit` → fork-bomb protection.
- `Ulimits` for `nofile` and `nproc`.

**Network**
- Attached only to `golaunch-edge`, with ICC disabled so containers can't reach each other.
- **No published ports whatsoever** — this is what the networking decision buys you. App containers are simply not addressable from the host.
- Firewall rules blocking egress from that network to host services (Postgres `5432`, Caddy admin `2019`) and to the cloud metadata endpoint `169.254.169.254` — that last one is the classic PaaS credential-theft vector.
- Outbound internet stays allowed (apps legitimately call third-party APIs).

**Restart policy**
- `RestartPolicy: "no"` — deliberately. Your `EventConsumer` owns restart decisions with its crash-count cap; letting Docker also auto-restart would fight it and hide crash loops.

## Phase 5 — Pipeline changes this forces

[deploy_pipeline.go](internal/application/deploy_pipeline.go) needs real edits — it's not a pure drop-in despite the interface seam:

1. **Dial target changes.** Line 163 currently does `RegisterRoute(slug, "localhost:<port>")`; it becomes the container name, `golaunch-<deploymentID>:3000`. The Caddy client already anticipates this in its own comments.
2. **Delete the port counter.** Lines 15–24 and the `"port"` label go away entirely — every container listens on 3000 internally. This removes the never-reused-ports leak by construction.
3. **🔒 Fix the env leak.** Line 146 passes `append(os.Environ(), ...)` into the app — meaning every deployed app currently inherits your control plane's entire environment, **including your Postgres credentials**. Under Docker this must become an explicit allowlist (`PORT`, `NODE_ENV`, plus per-project vars later). This is a live credential-disclosure bug today, and the migration is the natural place to fix it.
4. **Health check before the route flip.** `ContainerStart` returning ≠ the app is listening. Poll the container's port until it responds (or use a Docker `HEALTHCHECK`) before `RegisterRoute`, with a timeout that fails the deploy. Without this you'd flip traffic to a container that 502s for the first few seconds.
5. **Populate `BuildRequest.Limits`/`Timeout`** — currently left as zero values at line 131.

## Phase 6 — Wiring & config

- `config.go` gets a `RUNTIME=docker|hostexec` switch. `main.go` picks the implementation; **everything above the wiring stays untouched** — that's the payoff of the existing interface design.
- Keeping `hostexec` is nearly free and genuinely useful for local dev without a daemon. Default to `docker`.
- Docker client construction with API version negotiation + a health ping at boot, failing fast with a clear error if the socket isn't reachable.

## Phase 7 — Image lifecycle

- Image-per-deployment means disk grows unbounded → a GC pass keeping the last N images per project (`RemoveImage` already exists on the interface for exactly this).
- Rollback becomes nearly trivial once images are immutable and tagged per deployment: start the old image, flip the route. Worth doing as a follow-up right after this lands, since 90% of the machinery is the same.

---

## Suggested sequencing

| Step | Scope |
|---|---|
| 1 | Phase 0 host setup + Phase 1 detection extraction (low risk, unblocks everything) |
| 2 | Phase 2 builder + Phase 3 runtime, `RUNTIME=docker` off by default |
| 3 | Phase 4 hardening + Phase 5 pipeline edits (incl. the env-leak fix) |
| 4 | Flip default to docker, Phase 6/7 polish |

**Two caveats worth naming up front:** the env-leak fix in Phase 5 is a security bug that exists *right now* regardless of Docker — if you want, I can patch that independently before any of this. And I can't test against the daemon until the group/rootless setup in Phase 0 is done, so I'd be writing Phase 2–3 code I can compile but not run end-to-end until then.

Read it over — tell me to go and I'll start at step 1.