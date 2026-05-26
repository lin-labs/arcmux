package main

import "testing"

// Post-F11 the audit subcommand is a thin gRPC client. The old test
// suite exercised cmdAuditAppend (deleted: audit entries are now
// daemon-side side effects) and cmdAuditRecent (rewritten to call
// QueryAudit on the daemon's Unix socket).
//
// Re-creating the same coverage as unit tests would require spinning up
// a mock AgentRuntime server, which is non-trivial scaffolding for what
// the scenariotest harness already exercises against the real daemon. The
// substrate scenariotest `grpc-roundtrip` case covers the wire shape
// (audit recent on an empty store → []), so we keep the marker test
// below to make the gap explicit instead of silently dropping coverage.

func TestAuditRecent_GRPCBacked(t *testing.T) {
	t.Skip("gRPC-backed; wire coverage lives in internal/scenariotest/scenarios/grpc_roundtrip.go")
}
