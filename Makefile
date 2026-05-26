.PHONY: build install proto test validate validate-structural validate-substrate validate-substrate-e2e validate-e2e validate-eval validate-all clean start stop restart status logs tail release deploy

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
	go build -o bin/$(BINARY)-cli ./cmd/arcmux-cli
	go build -o bin/$(BINARY)-test ./cmd/arcmux-test
	go build -o bin/$(BINARY)-e2e ./cmd/arcmux-e2e

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 0755 bin/$(BINARY) $(BIN)
	install -m 0755 bin/$(BINARY)-cli $(INSTALL_DIR)/$(BINARY)-cli

proto:
	protoc -I proto \
		--go_out=gen --go_opt=paths=source_relative \
		--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
		arcmux/v1/arcmux.proto

test:
	go test ./...

# Per-commit gate: structural (gofmt + vet + go test + make build) AND
# substrate-behavioral (cmd/arcmux-test/ scenariotest cases spawning
# isolated daemons and asserting observable substrate effects), all in
# one ~12s pass via scripts/validate.sh. Free, fast.
#
# Convention: any Elon-turn cycle should end with `make validate` before commit.
validate: validate-structural

# The actual multi-step pass lives in scripts/validate.sh: gofmt, go vet,
# go test, make build, test:bootstrap, test:pulse-wake, test:grpc-rt. The
# script writes a structured JSON report under
# $ARCMUX_EPHEMERAL/validate-reports/ (or ./.validate-reports otherwise).
validate-structural:
	@./scripts/validate.sh

# Ad-hoc substrate-scenariotest runner — runs every cmd/arcmux-test
# scenario (no --scenario filter). The canonical per-commit gate
# (`make validate`) already runs all three substrate scenarios via the
# script; use this when you want to invoke the binary directly.
validate-substrate: build
	@./bin/$(BINARY)-test

# Back-compat alias for the old name. Prefer `validate-substrate` going forward.
validate-substrate-e2e: validate-substrate

# Big-feature gate: agent-behavioral end-to-end harness. Spawns real
# `claude -p` invocations against scenario prompts and validates the
# produced artifacts in a sandboxed workrepo. **Burns real Anthropic
# tokens.** Run intentionally — before a charter-level merge, after a
# substrate refactor that could break agent dispatch, before a release tag.
#
#   make validate-e2e                          # all scenarios
#   make validate-e2e SCENARIO=hello-server    # one scenario by name
#
# See cmd/arcmux-e2e/ and testdata/e2e-scenarios/.
validate-e2e: build
	@./bin/$(BINARY)-e2e $(if $(SCENARIO),--scenario $(SCENARIO))

# Back-compat aliases — keep one cycle while callers migrate.
validate-eval: validate-e2e
validate-all: validate

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
