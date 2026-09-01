package dockerbuild

import (
	"encoding/json"
	"fmt"
	"strings"

	"golaunch/internal/infrastructure/nodedetect"
)

// ContainerPort is the port every app listens on inside its own container.
// Containers are reached by name over the shared Docker network and never
// publish a host port, so there's no allocation to do — every app can use
// the same number.
const ContainerPort = 3000

// generatedDockerfileName is where the generated Dockerfile is written
// inside the build context. It's deliberately not "Dockerfile": a project
// that ships its own must not be clobbered, and this name makes it obvious
// in a build log that the file was synthesised rather than authored.
const generatedDockerfileName = ".golaunch.Dockerfile"

// baseImage pins the Node major the project asked for. Debian slim rather
// than Alpine on purpose: Alpine's musl libc breaks prebuilt native modules
// (sharp, bcrypt, canvas and friends), which on a platform accepting
// arbitrary user projects turns into a steady stream of build failures that
// are painful to diagnose. The size saving isn't worth it.
func baseImage(nodeMajor string) string {
	return fmt.Sprintf("node:%s-slim", nodeMajor)
}

// GenerateDockerfile renders a three-stage Dockerfile for the detected
// project.
//
// The staging exists for cache reuse: the deps stage copies only the
// manifest and lockfile, so its layer — the expensive `npm ci` — is reused
// across every deploy where dependencies didn't change, which is the single
// biggest build-time win available. Copying the source first would
// invalidate that layer on every commit.
func GenerateDockerfile(specs *nodedetect.ProjectSpecs) string {
	img := baseImage(specs.NodeMajor)

	var b strings.Builder

	// ── deps ──
	fmt.Fprintf(&b, "FROM %s AS deps\n", img)
	b.WriteString("WORKDIR /app\n")
	writeCorepack(&b, specs.PackageManager)
	b.WriteString("COPY package.json ./\n")
	if specs.Lockfile != "" {
		// only COPY a lockfile we know exists — COPY of a missing path is
		// a hard build failure
		fmt.Fprintf(&b, "COPY %s ./\n", specs.Lockfile)
	}
	fmt.Fprintf(&b, "RUN %s\n\n", shellJoin(specs.InstallCmd))

	// ── build ──
	fmt.Fprintf(&b, "FROM %s AS build\n", img)
	b.WriteString("WORKDIR /app\n")
	writeCorepack(&b, specs.PackageManager)
	b.WriteString("COPY --from=deps /app/node_modules ./node_modules\n")
	b.WriteString("COPY . .\n")
	if len(specs.BuildCmd) > 0 {
		fmt.Fprintf(&b, "RUN %s\n", shellJoin(specs.BuildCmd))
	}
	b.WriteString("\n")

	// ── runtime ──
	fmt.Fprintf(&b, "FROM %s AS runtime\n", img)
	b.WriteString("WORKDIR /app\n")
	// NODE_ENV is set only here, never in deps/build: exporting
	// production earlier makes npm skip devDependencies, which is exactly
	// what the build step needs.
	b.WriteString("ENV NODE_ENV=production\n")
	fmt.Fprintf(&b, "ENV PORT=%d\n", ContainerPort)
	// Next reads HOSTNAME to decide what to bind; without this its
	// standalone server binds localhost and is unreachable from the Caddy
	// container on the other side of the network.
	b.WriteString("ENV HOSTNAME=0.0.0.0\n")
	writeCorepack(&b, specs.PackageManager)
	b.WriteString("COPY --from=build --chown=node:node /app ./\n")
	// node images ship an unprivileged `node` user. Running as it means a
	// container escape starts from a non-root process, and it composes
	// with the daemon-level userns remap rather than replacing it.
	b.WriteString("USER node\n")
	fmt.Fprintf(&b, "EXPOSE %d\n", ContainerPort)
	fmt.Fprintf(&b, "CMD %s\n", execForm(specs.StartCmd))

	return b.String()
}

// writeCorepack enables the Node-bundled shim manager for pnpm and yarn.
// The node images ship npm only (plus a legacy yarn 1.x), so without this a
// pnpm project's install command isn't on PATH at all.
func writeCorepack(b *strings.Builder, pm nodedetect.PackageManager) {
	if pm == nodedetect.PackageManagerNPM {
		return
	}
	fmt.Fprintf(b, "RUN corepack enable %s\n", pm)
}

// execForm renders a command as a JSON array so Docker uses exec form.
// Shell form would wrap the process in /bin/sh, which does not forward
// SIGTERM to its child — the app would never run its shutdown handlers and
// would be SIGKILLed on every stop instead of draining.
func execForm(cmd []string) string {
	encoded, err := json.Marshal(cmd)
	if err != nil {
		// only possible on a nil slice, which the detector never returns
		return `["node","index.js"]`
	}
	return string(encoded)
}

// shellJoin renders a command for a RUN line. The detector produces fixed
// commands built from a closed set of literals — never user-controlled
// strings — so there's nothing here to quote-escape.
func shellJoin(cmd []string) string {
	return strings.Join(cmd, " ")
}
