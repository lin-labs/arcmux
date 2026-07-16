.PHONY: build install proto test validate validate-structural validate-substrate validate-substrate-e2e validate-e2e validate-e2e-hooks validate-eval validate-all clean start stop restart status logs tail release deploy service-install service-uninstall

BINARY := arcmux
INSTALL_DIR := $(HOME)/.local/bin
BIN := $(INSTALL_DIR)/$(BINARY)
LOG_DIR := $(HOME)/.config/arcmux/logs
LOG_FILE := $(LOG_DIR)/daemon.log

# Managed service definitions. `service-install` selects one at runtime.
LAUNCHD_LABEL := com.blin.arcmux
LAUNCHD_PLIST := $(HOME)/Library/LaunchAgents/$(LAUNCHD_LABEL).plist
LAUNCHD_TEMPLATE := packaging/launchd/$(LAUNCHD_LABEL).plist
SYSTEMD_UNIT := $(HOME)/.config/systemd/user/$(BINARY).service
SYSTEMD_TEMPLATE := packaging/systemd/$(BINARY).service
# PATH baked into the agent so the daemon can spawn tmux + coding agents
# (codex/claude/grok/node) with no login shell to inherit from.
SERVICE_PATH := $(HOME)/.local/bin:/opt/homebrew/bin:/opt/homebrew/opt/python@3.12/libexec/bin:$(HOME)/.grok/bin:$(HOME)/.cargo/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin

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

# Big-feature gate: agent-behavioral end-to-end harness. Drives scenarios
# through the full stack — arcmux daemon + elonco service + arcmux-spawned
# claude agent — and validates the produced artifacts in a sandboxed
# workrepo. **Burns real Anthropic tokens.** Run intentionally — before a
# charter-level merge, after a substrate refactor that could break agent
# dispatch, before a release tag.
#
#   make validate-e2e                                   # all scenarios via elonco
#   make validate-e2e SCENARIO=hello-server             # one scenario by name
#   make validate-e2e MODE=direct                       # bypass elonco: plain claude -p
#
# See cmd/arcmux-e2e/ and testdata/e2e-scenarios/. The `elonco` mode
# requires the sibling elonco repo at /Users/blin/Projects/elonco/ to
# be installed (its `.venv/bin/python` is auto-detected); fall back to
# `MODE=direct` if you just want to exercise plain claude dispatch.
validate-e2e: build
	@./bin/$(BINARY)-e2e \
	  $(if $(SCENARIO),--scenario $(SCENARIO)) \
	  $(if $(MODE),--mode $(MODE))

# Real-agent e2e for the hooks delivery judge. Spawns a LIVE agent through an
# isolated daemon configured with [delivery].judge=hooks and asserts (1) the
# agent's native hook fired `arcmux hook` (state doc records prompt_submit) and
# (2) a prompt_ingested event carries judge_source=hooks. Opt-in / not CI:
# needs a real agent binary + auth + the hook registered in the agent's config
# (claude: ~/.claude/settings.json; codex: ~/.codex/hooks.json + /hooks trust).
#
#   make validate-e2e-hooks                # claude (default)
#   make validate-e2e-hooks AGENT=codex    # codex (after trusting its hooks)
validate-e2e-hooks: build
	@bash scripts/e2e/hooks-judge-live.sh $(if $(AGENT),$(AGENT),claude)

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

# If the launchd LaunchAgent plist exists (macOS), defer process control to
# launchctl (like the systemd path above) so the Makefile doesn't fight the
# agent's KeepAlive respawn. File-based check (not "is it loaded") mirrors the
# systemd unit-file check, so stop→start round-trips still route to launchd.
HAS_LAUNCHD_AGENT = test -f $(LAUNCHD_PLIST)

start:
	@mkdir -p $(LOG_DIR)
	@if $(HAS_SYSTEMD_UNIT); then \
		systemctl --user start $(BINARY); \
		echo "started $(BINARY) via systemd"; \
	elif $(HAS_LAUNCHD_AGENT); then \
		launchctl bootstrap gui/$$(id -u) $(LAUNCHD_PLIST) 2>/dev/null \
			|| launchctl kickstart gui/$$(id -u)/$(LAUNCHD_LABEL); \
		echo "started $(BINARY) via launchd"; \
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
	elif $(HAS_LAUNCHD_AGENT); then \
		launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null || true; \
		echo "stopped $(BINARY) via launchd (run 'make start' to reload)"; \
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
	elif $(HAS_LAUNCHD_AGENT); then \
		launchctl kickstart -k gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null \
			|| launchctl bootstrap gui/$$(id -u) $(LAUNCHD_PLIST); \
		echo "restarted $(BINARY) via launchd"; \
	else \
		$(MAKE) --no-print-directory stop start; \
	fi

# Install (or refresh) the native managed service. Linux uses a systemd user
# unit whose KillMode deliberately preserves the external tmux server across
# daemon restarts; macOS retains the launchd LaunchAgent behavior.
service-install: install
	@test -x $(BIN) || { echo "missing $(BIN); run 'make install' first"; exit 1; }
	@set -e; case "$$(uname -s)" in \
	  Darwin) \
	    mkdir -p $(HOME)/Library/LaunchAgents $(LOG_DIR); \
	    sed -e 's#@HOME@#$(HOME)#g' \
	        -e 's#@BIN@#$(BIN)#g' \
	        -e 's#@LOG_FILE@#$(LOG_FILE)#g' \
	        -e 's#@PATH@#$(SERVICE_PATH)#g' \
	        $(LAUNCHD_TEMPLATE) > $(LAUNCHD_PLIST); \
	    launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null || true; \
	    pkill -TERM -x $(BINARY) 2>/dev/null || true; \
	    sleep 1; \
	    launchctl bootstrap gui/$$(id -u) $(LAUNCHD_PLIST); \
	    launchctl enable gui/$$(id -u)/$(LAUNCHD_LABEL); \
	    launchctl kickstart gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null || true; \
	    echo "installed launchd agent $(LAUNCHD_LABEL) -> $(LAUNCHD_PLIST)"; \
	    ;; \
	  Linux) \
	    was_active=false; \
	    if systemctl --user is-active --quiet $(BINARY).service; then \
	      was_active=true; \
	    fi; \
	    mkdir -p $(dir $(SYSTEMD_UNIT)); \
	    sed -e 's#@HOME@#$(HOME)#g' \
	        -e 's#@BIN@#$(BIN)#g' \
	        -e 's#@PATH@#$(SERVICE_PATH)#g' \
	        $(SYSTEMD_TEMPLATE) > $(SYSTEMD_UNIT); \
	    systemctl --user daemon-reload; \
	    if [ "$$was_active" != true ]; then \
	      pkill -TERM -x $(BINARY) 2>/dev/null || true; \
	      for i in 1 2 3 4 5; do \
	        pgrep -x $(BINARY) >/dev/null || break; \
	        sleep 1; \
	      done; \
	      if pgrep -x $(BINARY) >/dev/null; then \
	        pkill -KILL -x $(BINARY); \
	      fi; \
	    fi; \
	    systemctl --user enable $(BINARY).service; \
	    systemctl --user restart $(BINARY).service; \
	    echo "installed systemd user unit $(BINARY).service -> $(SYSTEMD_UNIT)"; \
	    ;; \
	  *) \
	    echo "unsupported service platform: $$(uname -s)" >&2; \
	    exit 1; \
	    ;; \
	esac

service-uninstall:
	@set -e; case "$$(uname -s)" in \
	  Darwin) \
	    launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL) 2>/dev/null || true; \
	    rm -f $(LAUNCHD_PLIST); \
	    echo "removed launchd agent $(LAUNCHD_LABEL)"; \
	    ;; \
	  Linux) \
	    load_state="$$(systemctl --user show --property LoadState --value $(BINARY).service 2>/dev/null || true)"; \
	    if [ -f $(SYSTEMD_UNIT) ] || { [ -n "$$load_state" ] && [ "$$load_state" != not-found ]; }; then \
	      systemctl --user disable --now $(BINARY).service; \
	    fi; \
	    rm -f $(SYSTEMD_UNIT); \
	    systemctl --user daemon-reload; \
	    echo "removed systemd user unit $(BINARY).service"; \
	    ;; \
	  *) \
	    echo "unsupported service platform: $$(uname -s)" >&2; \
	    exit 1; \
	    ;; \
	esac

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

# Local-on-target release: Linux must install/reload the safe systemd unit
# before its first restart. Darwin keeps the established install + launchd
# restart path. Run this on the runtime host, or via `make deploy`.
ifeq ($(shell uname -s),Linux)
release: service-install
else
release: install restart
endif
	@echo "released $(BINARY) at $(BIN)"

# Remote deploy: ssh to LABS_HOST, fast-forward git pull, then run `make release`.
# Run from your workstation. Override LABS_HOST / LABS_REPO as needed.
deploy:
	@echo "deploying to $(LABS_HOST):$(LABS_REPO)"
	@ssh $(LABS_HOST) 'set -e; \
		export PATH=/usr/local/go/bin:$$HOME/.local/bin:$$PATH; \
		cd $(LABS_REPO) && git pull --ff-only && make release'
