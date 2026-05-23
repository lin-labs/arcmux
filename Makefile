.PHONY: build install proto test clean start stop restart status logs tail release deploy

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

install: build
	mkdir -p $(INSTALL_DIR)
	cp bin/$(BINARY) $(INSTALL_DIR)/$(BINARY)

proto:
	protoc \
		--go_out=gen --go_opt=paths=source_relative \
		--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
		proto/arcmux/v1/arcmux.proto

test:
	go test ./...

clean:
	rm -rf bin/ gen/

deps:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

start:
	@mkdir -p $(LOG_DIR)
	@if pgrep -x $(BINARY) >/dev/null; then \
		echo "$(BINARY) already running (pids $$(pgrep -x $(BINARY) | tr '\n' ' '))"; \
	else \
		test -x $(BIN) || { echo "missing $(BIN); run 'make install' first"; exit 1; }; \
		nohup $(BIN) start >> $(LOG_FILE) 2>&1 & \
		sleep 1; \
		echo "started $(BINARY) (pid $$(pgrep -x $(BINARY)))"; \
	fi

stop:
	@if pgrep -x $(BINARY) >/dev/null; then \
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

restart: stop start

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
	@ssh $(LABS_HOST) 'set -e; cd $(LABS_REPO) && git pull --ff-only && make release'
