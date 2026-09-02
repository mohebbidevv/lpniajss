NETWORK ?= golaunch-edge
NODE_IMAGE ?= node:22-slim

.PHONY: run run-hostexec setup network pull caddy caddy-stop caddy-reload doctor test clean-images

## run the control plane (driver comes from cmd/configuration.json)
run:
	go run ./cmd

## run against host processes instead of Docker, for a box with no daemon
run-hostexec:
	GOLAUNCH_RUNTIME=hostexec go run ./cmd

## one-time host prerequisites
setup: network pull
	@echo "host ready — start the proxy with 'make caddy'"

## shared bridge every app container joins; the control plane also creates
## this at boot, so this target is only for setting up ahead of time
network:
	@docker network inspect $(NETWORK) >/dev/null 2>&1 \
		|| docker network create --driver bridge $(NETWORK)

## pre-pull the base image so the first deploy isn't waiting on a registry
pull:
	docker pull $(NODE_IMAGE)

## Caddy runs on the host: its admin API stays on 127.0.0.1, unreachable
## from any container, and the host routes to the bridge subnet directly so
## it can still dial app containers by IP with nothing published.
caddy:
	caddy start --config Caddyfile

caddy-stop:
	caddy stop

caddy-reload:
	caddy reload --config Caddyfile

test:
	go test ./...

## check the host is actually ready to deploy
doctor:
	@docker info >/dev/null 2>&1 && echo "docker daemon: ok" || echo "docker daemon: UNREACHABLE"
	@docker network inspect $(NETWORK) >/dev/null 2>&1 \
		&& echo "network $(NETWORK): ok" || echo "network $(NETWORK): missing (run 'make network')"
	@curl -fsS http://localhost:2019/config/ >/dev/null 2>&1 \
		&& echo "caddy admin: ok" || echo "caddy admin: UNREACHABLE (run 'make caddy')"

## drop images from deployments that are no longer referenced
clean-images:
	@docker image ls --filter 'reference=golaunch/*' --format '{{.Repository}}:{{.Tag}}'
