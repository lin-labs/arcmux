package main

import "testing"

// Post-F11 the inbox subcommands (push/peek/ack) are thin gRPC clients
// against Send / PeekInbox / AckInbox. The old direct-bbolt test suite
// exercised the store wrapper functions; the gRPC-shaped equivalents
// are covered by the substrate scenariotest `grpc-roundtrip` case against a
// real daemon, since mocking the AgentRuntime server here would be
// disproportionate scaffolding for the surface we're testing.
//
// We keep marker tests below so the coverage gap is visible.

func TestInboxPush_GRPCBacked(t *testing.T) {
	t.Skip("gRPC-backed; wire coverage lives in internal/scenariotest/scenarios/grpc_roundtrip.go")
}

func TestInboxPeek_GRPCBacked(t *testing.T) {
	t.Skip("gRPC-backed; wire coverage lives in internal/scenariotest/scenarios/grpc_roundtrip.go")
}

func TestInboxAck_GRPCBacked(t *testing.T) {
	t.Skip("gRPC-backed; wire coverage lives in internal/scenariotest/scenarios/grpc_roundtrip.go")
}
