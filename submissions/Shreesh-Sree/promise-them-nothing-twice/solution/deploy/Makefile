COMPOSE = docker compose -f docker-compose.yml

.PHONY: up down logs harness harness-json build

up:
	$(COMPOSE) up -d --build node1 node2 node3 nginx
	@echo "waiting for nodes to come up..."
	@for i in 1 2 3; do \
		until curl -sf http://localhost:8080/internal/healthz >/dev/null; do sleep 1; done; \
	done
	@echo "up. MODE=$${COORDINATOR_MODE:-static}"

down:
	$(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f

# The verification harness. Runs on the host, not in a container: two of
# its five scenarios (node-failure's kill/revive, and the optional
# server-log cross-check) shell out to `docker compose`, which needs a
# real docker CLI and this compose file — a containerized harness would
# need the docker socket mounted in just to control its own siblings, for
# no benefit over running the Go binary directly against the already
# host-exposed nginx port (localhost:8080).
#
# Usage:
#   make harness                              # all 5 scenarios
#   make harness SCENARIOS=window-boundary     # just one
#   make harness SCENARIOS=northwind-batch     # see deploy/README or
#                                               # up-northwind-window below
#                                               # for exercising the
#                                               # override-active phase
harness:
	cd .. && go run ./cmd/harness \
		-url http://localhost:8080 \
		-config configs/customers.yaml \
		-compose-file deploy/docker-compose.yml \
		-services node1,node2,node3 \
		-kill-cmd "docker compose -f deploy/docker-compose.yml stop node2" \
		-revive-cmd "docker compose -f deploy/docker-compose.yml start node2" \
		-scenarios $${SCENARIOS:-all}

harness-json:
	cd .. && go run ./cmd/harness \
		-url http://localhost:8080 \
		-config configs/customers.yaml \
		-compose-file deploy/docker-compose.yml \
		-services node1,node2,node3 \
		-kill-cmd "docker compose -f deploy/docker-compose.yml stop node2" \
		-revive-cmd "docker compose -f deploy/docker-compose.yml start node2" \
		-scenarios $${SCENARIOS:-all} \
		-json

# Bring the stack up with Northwind's override window already active, so
# northwind-batch's override-active phase is reachable without waiting
# for real UTC 02:00-04:00. See internal/policy/devclock.go.
up-northwind-window:
	RELAYAPI_DEV_CLOCK_AS_OF=2026-08-11T02:30:00Z $(COMPOSE) up -d --build node1 node2 node3 nginx
	@echo "waiting for nodes to come up..."
	@for i in 1 2 3; do \
		until curl -sf http://localhost:8080/internal/healthz >/dev/null; do sleep 1; done; \
	done
	@echo "up, dev-clock pinned inside Northwind's override window (02:30 UTC)."

# Bring up the stack with the deliberately broken fixed-window limiter.
# Requires a rebuild with -tags=fixedwindow so the broken code is actually
# compiled in — a normal build panics on COORDINATOR_MODE=fixedwindow with
# a "rebuild with -tags fixedwindow" error, by design.
up-fixedwindow:
	BUILD_TAGS=fixedwindow COORDINATOR_MODE=fixedwindow $(COMPOSE) up -d --build node1 node2 node3 nginx
	@echo "waiting for nodes to come up..."
	@for i in 1 2 3; do \
		until curl -sf http://localhost:8080/internal/healthz >/dev/null; do sleep 1; done; \
	done
	@echo "up, FIXED-WINDOW MODE (deliberately broken — harness should catch it)."

build:
	$(COMPOSE) build
