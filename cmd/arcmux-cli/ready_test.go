package main

import "testing"

// `arcmux-cli ready` is a thin gRPC client for the daemon's Ready RPC.
// Wire coverage lives in the substrate-e2e `grpc-roundtrip` scenario,
// which exercises Ready against a real daemon. A unit-level test would
// require a mock AgentRuntime server — disproportionate for a one-RPC
// subcommand.

func TestReady_GRPCBacked(t *testing.T) {
	t.Skip("gRPC-backed; wire coverage lives in internal/e2e/scenarios/grpc_roundtrip.go")
}
