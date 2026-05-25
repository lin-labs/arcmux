.PHONY: build install proto test validate validate-e2e validate-eval validate-all clean start stop restart status logs tail release deploy

BINARY := arcmux
INSTALL_DIR := $(HOME)/.local/bin
BIN := $(INSTALL_DIR)/$(BINARY)
LOG_DIR := $(HOME)/.config/arcmux/logs
LOG_FILE := $(LOG_DIR)/daemon.log

# Remote deploy target — override on the command line, e.g.
#   make deploy LABS_HOST=labs LABS_REPO=~/Projects/arcmux
LABS_HOST ?= labs
LABS_REPO ?= ~/Projects/arcmux

build:
	go build -o bin/$(BINARY) ./cmd/arcmux
	go build -o bin/$(BINARY)-call ./cmd/arcmux-call
	go build -o bin/$(BINARY)-e2e ./cmd/arcmux-e2e
	go build -o bin/$(BINARY)-eval ./cmd/arcmux-eval

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 0755 bin/$(BINARY) $(BIN)
	install -m 0755 bin/$(BINARY)-call $(INSTALL_DIR)/$(BINARY)-call

proto:
	protoc -I proto \
		--go_out=gen --go_opt=paths=source_relative \
		--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
		arcmux/v1/arcmux.proto

test:
	go test ./...

# Self-validating dev cycle: gofmt + go vet + go test + build + e2e smoke
# against the built binaries; writes a structured JSON report under
# $ARCMUX_EPHEMERAL/validate-reports/ (or ./.validate-reports if unset).
# Convention: any Elon-turn cycle should end with `make validate` before commit.
validate:
	@./scripts/validate.sh

# Behavioral end-to-end harness: SETUP→ACT→ASSERT→TEARDOWN scenarios that
# spawn isolated daemons / scaffold throwaway projects and assert the
# substrate observably DID the right thing. See cmd/arcmux-e2e/.
# Requires `make build` first; the harness invokes ./bin/arcmux* directly.
validate-e2e: build
	@./bin/$(BINARY)-e2e

# Full validation pass: structural (validate.sh) AND behavioral (e2e). Use
# this on Elon-turn cycles that touched substrate code or wire shape.
#
# Note: validate-all does NOT include validate-eval. eval spends real tokens
# (spawns claude headless against scenarios in testdata/eval-scenarios/) and
# is run on demand, not per-commit.
validate-all: validate validate-e2e

# Agent-behavioral eval harness: spawns real claude -p invocations against
# scenario prompts, validates artifacts in a sandboxed workrepo. Optional
# SCENARIO= filter selects one or more scenario names; default runs all.
# Burns tokens — run intentionally, not as part of the per-commit gate.
#
#   make validate-eval                          # all scenarios
#   make validate-eval SCENARIO=hello-server    # one scenario by name
#
# See cmd/arcmux-eval/ and testdata/eval-scenarios/.
validate-eval: build
	@./bin/$(BINARY)-eval $(if $(SCENARIO),--scenario $(SCENARIO))

clean:
	rm -rf bin/ gen/

deps:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# If a `<service>.service` user unit exists, defer process control to systemd
# so the Makefile doesn't fight Restart=always. Detected at recipe time so a
# unit added later just works without re-editing the Makefile.
HAS_SYSTEMD_UNIT = systemctl --user list-unit-files $(BINARY).service 2>/dev/null \
	| grep -q '^$(BINARY)\.service'

start:
	@mkdir -p $(LOG_DIR)
	@if $(HAS_SYSTEMD_UNIT); then \
		systemctl --user start $(BINARY); \
		echo "started $(BINARY) via systemd"; \
	elif pgrep -x $(BINARY) >/dev/null; then \
		echo "$(BINARY) already running (pids $$(pgrep -x $(BINARY) | tr '\n' ' '))"; \
	else \
		test -x $(BIN) || { echo "missing $(BIN); run 'make install' first"; exit 1; }; \
		nohup $(BIN) start >> $(LOG_FILE) 2>&1 & \
		sleep 1; \
		echo "started $(BINARY) (pid $$(pgrep -x $(BINARY)))"; \
	fi

stop:
	@if $(HAS_SYSTEMD_UNIT); then \
		systemctl --user stop $(BINARY); \
		echo "stopped $(BINARY) via systemd"; \
	elif pgrep -x $(BINARY) >/dev/null; then \
		pkill -TERM -x $(BINARY); \
		for i in 1 2 3 4 5; do \
			pgrep -x $(BINARY) >/dev/null || break; \
			sleep 1; \
		done; \
		if pgrep -x $(BINARY) >/dev/null; then \
			echo "graceful stop timed out, sending SIGKILL"; \
			pkill -KILL -x $(BINARY); \
		fi; \
		echo "stopped $(BINARY)"; \
	else \
		echo "$(BINARY) not running"; \
	fi

# When systemd manages the service, use `restart` directly — one transaction,
# avoids the stop→start race where another supervisor (or us) can squeeze a
# duplicate daemon in between.
restart:
	@if $(HAS_SYSTEMD_UNIT); then \
		systemctl --user restart $(BINARY); \
		echo "restarted $(BINARY) via systemd"; \
	else \
		$(MAKE) --no-print-directory stop start; \
	fi

status:
	@pids="$$(pgrep -x $(BINARY))"; \
	if [ -n "$$pids" ]; then \
		count=$$(echo "$$pids" | wc -l); \
		echo "$(BINARY) running ($$count process(es)):"; \
		ps -o pid,etime,args -p $$pids; \
	else \
		echo "$(BINARY) not running"; \
	fi

logs:
	@test -f $(LOG_FILE) && less +G $(LOG_FILE) || echo "no log file at $(LOG_FILE)"

tail:
	@mkdir -p $(LOG_DIR)
	@touch $(LOG_FILE)
	@tail -f $(LOG_FILE)

# Local-on-target release: rebuild binary, install to ~/.local/bin, restart daemon.
# Run this on the host where arcmux runs (labs), or via `make deploy` from elsewhere.
release: install restart
	@echo "released $(BINARY) at $(BIN)"

# Remote deploy: ssh to LABS_HOST, fast-forward git pull, then run `make release`.
# Run from your workstation. Override LABS_HOST / LABS_REPO as needed.
deploy:
	@echo "deploying to $(LABS_HOST):$(LABS_REPO)"
	@ssh $(LABS_HOST) 'set -e; \
		export PATH=/usr/local/go/bin:$$HOME/.local/bin:$$PATH; \
		cd $(LABS_REPO) && git pull --ff-only && make release'
