.PHONY: build install proto test clean

BINARY := arcmux
INSTALL_DIR := ~/.local/bin

build:
	go build -o bin/$(BINARY) ./cmd/atrs

install: build
	mkdir -p $(INSTALL_DIR)
	cp bin/$(BINARY) $(INSTALL_DIR)/$(BINARY)

proto:
	protoc \
		--go_out=gen --go_opt=paths=source_relative \
		--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
		proto/atrs/v1/atrs.proto

test:
	go test ./...

clean:
	rm -rf bin/ gen/

deps:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
