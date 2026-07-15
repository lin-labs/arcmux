package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/hooks"
	arcmuxmesh "github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/meshstate"
	"github.com/lin-labs/arcmux/internal/session"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

func TestMeshApplicationSessionsArtifactsAndExplicitEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")

	launchCWD := filepath.Join(home, "Projects", "arcmux")
	managed := session.NewSession("s-shared", "mesh worker", "codex", launchCWD)
	managed.SetTransport("tmux")
	managed.SetOwnerID("arcmux:test")
	managed.SetState(session.StateIdle)
	managed.IncrementNudge()
	managed.IncrementNudge()
	server.mu.Lock()
	server.sessions[managed.Snapshot().ID] = managed
	server.mu.Unlock()
	if err := hooks.ApplyEventWithContract(
		server.cfg.Hooks.SessionStateDir, "s-shared", "codex", hooks.EventPromptSubmit, "",
		hooks.TurnContractUpdate{Goal: "do not leak flurple-zebra-7391", OverallGoal: "raw launch flurple-zebra-7391"}, time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	emptyProfile := newCatalogTestDaemon(t, "empty")
	server.profileManager = &ProfileManager{daemons: map[string]*Daemon{"empty": emptyProfile}, records: map[string]ProfileRecord{}}

	serverStore, _ := server.meshStateStore()
	clientStore, _ := client.meshStateStore()
	if err := serverStore.PutArtifact(meshstate.ArtifactEnvelope{
		ID: "same", Kind: meshstate.ArtifactPullRequest, Title: "Server PR",
		URL: "https://github.com/lin-labs/arcmux/pull/3", Provenance: "local-test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := clientStore.PutArtifact(meshstate.ArtifactEnvelope{
		ID: "same", Kind: meshstate.ArtifactPullRequest, Title: "Client PR",
		URL: "https://github.com/lin-labs/arcmux/pull/4", Provenance: "local-test",
	}); err != nil {
		t.Fatal(err)
	}

	serverManager, clientManager := startDaemonMeshPair(t, server, client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	local := server.localMeshSessions()
	projections, err := client.RemoteSessionsList(ctx, "server")
	if err != nil {
		t.Fatalf("RemoteSessionsList: %v", err)
	}
	if len(projections) != 1 {
		t.Fatalf("remote projections = %d, want 1: %#v", len(projections), projections)
	}
	if projections[0].Locator.DeviceID != "server" || projections[0].Freshness != meshstate.FreshnessFresh {
		t.Fatalf("projection identity/freshness = %#v", projections[0])
	}
	var remoteSummary sessionview.Summary
	if err := json.Unmarshal(projections[0].Metadata, &remoteSummary); err != nil {
		t.Fatal(err)
	}
	if len(local.Sessions) != 1 {
		t.Fatalf("local safe sessions = %#v", local.Sessions)
	}
	if remoteSummary.Name != local.Sessions[0].Name || remoteSummary.State != local.Sessions[0].State || remoteSummary.LaunchCWD != "~/Projects/arcmux" {
		t.Fatalf("remote semantic summary = %#v, local = %#v", remoteSummary, local.Sessions[0])
	}
	if remoteSummary.Work != nil || remoteSummary.CurrentWork != nil || strings.Contains(string(projections[0].Metadata), "flurple-zebra-7391") {
		t.Fatalf("unproven turn-contract text crossed mesh: %s", projections[0].Metadata)
	}
	peerState, err := clientStore.GetPeer("server")
	if err != nil || peerState.Freshness != meshstate.FreshnessFresh {
		t.Fatalf("peer state = %#v err=%v", peerState, err)
	}
	if _, ok := peerState.Inventories["profile:empty"]; !ok {
		t.Fatalf("empty profile inventory not committed: %#v", peerState.Inventories)
	}
	if peerState.SessionInventory == nil {
		t.Fatalf("atomic session inventory cursor missing: %#v", peerState)
	}
	detail, err := client.RemoteSessionGet(ctx, "server", sessionview.RootProfileScope, "s-shared")
	if err != nil || detail.NudgeCount != 2 {
		t.Fatalf("live remote detail = %#v err=%v", detail, err)
	}
	clientHTTP := NewHTTPServer(client, "127.0.0.1:0")
	liveDetail := meshHTTPRequest(clientHTTP, http.MethodGet, "/mesh/session?peer=server&profile=root&session=s-shared&live=1", nil)
	if liveDetail.Code != http.StatusOK || !strings.Contains(liveDetail.Body.String(), `"nudge_count":2`) {
		t.Fatalf("live session HTTP status=%d body=%s", liveDetail.Code, liveDetail.Body.String())
	}
	liveArtifact := meshHTTPRequest(clientHTTP, http.MethodGet, "/mesh/artifact?peer=server&kind=pull_request&id=same&live=1", nil)
	if liveArtifact.Code != http.StatusOK || !strings.Contains(liveArtifact.Body.String(), `"origin_device_id":"server"`) ||
		strings.Contains(liveArtifact.Body.String(), `"url"`) ||
		strings.Contains(liveArtifact.Body.String(), "Server PR") {
		t.Fatalf("live artifact HTTP status=%d body=%s", liveArtifact.Code, liveArtifact.Body.String())
	}

	if err := hooks.ApplyContractOnly(
		server.cfg.Hooks.SessionStateDir, "s-shared", "codex",
		hooks.TurnContractUpdate{
			OverallGoal:           "Render remote surfaces as native Mission Control sessions",
			OverallGoalProvenance: hooks.OverallGoalSummarizerProvenance,
		}, time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	projections, err = client.RemoteSessionsList(ctx, "server")
	if err != nil || len(projections) != 1 {
		t.Fatalf("summarized RemoteSessionsList=%#v err=%v", projections, err)
	}
	if err := json.Unmarshal(projections[0].Metadata, &remoteSummary); err != nil {
		t.Fatal(err)
	}
	if remoteSummary.CurrentWork == nil ||
		remoteSummary.CurrentWork.Summary != "Render remote surfaces as native Mission Control sessions" ||
		remoteSummary.CurrentWork.Provenance != hooks.OverallGoalSummarizerProvenance {
		t.Fatalf("proven current work did not cross mesh: %s", projections[0].Metadata)
	}
	if remoteSummary.Work != nil || strings.Contains(string(projections[0].Metadata), "flurple-zebra-7391") {
		t.Fatalf("local-only work leaked beside current_work: %s", projections[0].Metadata)
	}

	server.mu.Lock()
	delete(server.sessions, "s-shared")
	server.mu.Unlock()
	projections, err = client.RemoteSessionsList(ctx, "server")
	if err != nil {
		t.Fatalf("second RemoteSessionsList: %v", err)
	}
	if len(projections) != 1 || projections[0].Freshness != meshstate.FreshnessGone {
		t.Fatalf("omitted projection did not become gone: %#v", projections)
	}

	remoteArtifacts, err := client.RemoteArtifactsList(ctx, "server", "")
	if err != nil {
		t.Fatalf("RemoteArtifactsList: %v", err)
	}
	if len(remoteArtifacts) != 1 || remoteArtifacts[0].ID != "same" || remoteArtifacts[0].SourceID != "same" || remoteArtifacts[0].OriginDeviceID != "server" {
		t.Fatalf("remote artifact origin was not authenticated/store-stamped: %#v", remoteArtifacts)
	}
	clientArtifacts, _ := clientStore.ListAllArtifacts(meshstate.ArtifactPullRequest)
	if len(clientArtifacts) != 2 {
		t.Fatalf("local/remote artifact collision: %#v", clientArtifacts)
	}
	remoteArtifactList := meshHTTPRequest(clientHTTP, http.MethodGet, "/mesh/artifacts?peer=server&kind=pull_request", nil)
	if remoteArtifactList.Code != http.StatusOK || !strings.Contains(remoteArtifactList.Body.String(), `"origin_device_id":"server"`) ||
		strings.Contains(remoteArtifactList.Body.String(), "Client PR") || strings.Contains(remoteArtifactList.Body.String(), "Server PR") {
		t.Fatalf("origin-filtered artifact list status=%d body=%s", remoteArtifactList.Code, remoteArtifactList.Body.String())
	}
	cachedArtifact := meshHTTPRequest(clientHTTP, http.MethodGet, "/mesh/artifact?peer=server&kind=pull_request&id=same", nil)
	if cachedArtifact.Code != http.StatusOK || !strings.Contains(cachedArtifact.Body.String(), `"source_id":"same"`) {
		t.Fatalf("cached remote artifact status=%d body=%s", cachedArtifact.Code, cachedArtifact.Body.String())
	}
	if _, err := server.RemoteArtifactsList(ctx, "client", ""); err != nil {
		t.Fatalf("reverse artifact list: %v", err)
	}
	serverArtifacts, _ := serverStore.ListAllArtifacts(meshstate.ArtifactPullRequest)
	if len(serverArtifacts) != 2 {
		t.Fatalf("origin-separated artifact inventory or echo suppression failed: %#v", serverArtifacts)
	}

	eventStream, cancelEventStream := clientManager.SubscribeEvents(8)
	defer cancelEventStream()
	managed = session.NewSession("s-event", "event worker", "codex", launchCWD)
	managed.SetState(session.StateIdle)
	server.mu.Lock()
	server.sessions[managed.Snapshot().ID] = managed
	server.mu.Unlock()
	server.emitStateChanged("s-event", session.StateIdle, "must-not-cross-before-subscribe")
	select {
	case event := <-eventStream:
		t.Fatalf("event delivered before explicit subscribe: %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
	if topics, err := client.SubscribeMeshPeer(ctx, "server", []string{meshTopicSessions}); err != nil || len(topics) != 1 {
		t.Fatalf("SubscribeMeshPeer = %v, %v", topics, err)
	}
	server.emitStateChanged("s-event", session.StateWorking, "top-secret-message")
	var received arcmuxmesh.PeerEvent
	select {
	case received = <-eventStream:
	case <-ctx.Done():
		t.Fatal("subscribed session event not received")
	}
	if received.Event.Name != "sessions.changed" || strings.Contains(string(received.Event.Data), "top-secret-message") {
		t.Fatalf("unsafe or unexpected event: %#v data=%s", received, received.Event.Data)
	}
	var safeEvent meshSessionEvent
	if err := json.Unmarshal(received.Event.Data, &safeEvent); err != nil || safeEvent.Session == nil || safeEvent.SessionID != "s-event" {
		t.Fatalf("safe session event = %#v err=%v", safeEvent, err)
	}
	clientConnection, ok := meshConnectedAt(serverManager, "client")
	if !ok {
		t.Fatal("client connection missing for gap retry")
	}
	server.markMeshGap("client", clientConnection)
	server.retryMeshGaps(serverManager)
	select {
	case retriedGap := <-eventStream:
		if retriedGap.Event.Name != "events.gap" || !strings.Contains(string(retriedGap.Event.Data), "sender_backpressure") {
			t.Fatalf("retried sender gap = %#v", retriedGap)
		}
	case <-ctx.Done():
		t.Fatal("sender backpressure gap was not retried")
	}
	profileSession := session.NewSession("s-profile", "profile worker", "codex", launchCWD)
	profileSession.SetState(session.StateIdle)
	emptyProfile.mu.Lock()
	emptyProfile.sessions[profileSession.Snapshot().ID] = profileSession
	emptyProfile.mu.Unlock()
	server.forwardProfileSessionEvents(ctx, "empty", emptyProfile)
	emptyProfile.emitStateChanged("s-profile", session.StateWorking, "profile-internal-message")
	select {
	case profileEvent := <-eventStream:
		var payload meshSessionEvent
		if err := json.Unmarshal(profileEvent.Event.Data, &payload); err != nil || payload.ProfileScope != "profile:empty" || payload.SessionID != "s-profile" {
			t.Fatalf("profile event = %#v payload=%#v err=%v", profileEvent, payload, err)
		}
	case <-ctx.Done():
		t.Fatal("profile session event not received")
	}
	if err := client.UnsubscribeMeshPeer(ctx, "server"); err != nil {
		t.Fatal(err)
	}
	if topics, err := clientStore.DesiredTopics("server"); err != nil || len(topics) != 0 {
		t.Fatalf("unsubscribe did not clear durable intent: topics=%v err=%v", topics, err)
	}
	server.emitStateChanged("s-event", session.StateIdle, "after-unsubscribe")
	select {
	case event := <-eventStream:
		t.Fatalf("event delivered after unsubscribe: %#v", event)
	case <-time.After(100 * time.Millisecond):
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	serverManager.Stop(stopCtx)
	stopCancel()
	waitMeshPeerFreshness(t, clientStore, "server", meshstate.FreshnessStale)
}

func TestMeshPaginatedInventoriesStayBelowTransportBounds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	const sessionCount = 50
	const artifactCount = 40
	for i := 0; i < sessionCount; i++ {
		id := fmt.Sprintf("s-page-%02d", i)
		managed := session.NewSession(id, id, "codex", filepath.Join(home, strings.Repeat("c", 2000)))
		managed.SetOwnerID(strings.Repeat("o", 900))
		managed.SetState(session.StateIdle)
		server.mu.Lock()
		server.sessions[id] = managed
		server.mu.Unlock()
	}
	serverStore, _ := server.meshStateStore()
	for i := 0; i < artifactCount; i++ {
		if err := serverStore.PutArtifact(meshstate.ArtifactEnvelope{
			ID: fmt.Sprintf("doc-%02d", i), Kind: meshstate.ArtifactDocument,
			Title: strings.Repeat("t", 500), PathHint: "~/" + strings.Repeat("p", 900),
			URL: "https://example.com/" + strings.Repeat("u", 1900), Provenance: strings.Repeat("v", 400),
		}); err != nil {
			t.Fatal(err)
		}
	}

	assertSessionPagesBounded(t, server, "byte-probe", sessionCount)
	assertArtifactPagesBounded(t, server, "byte-probe", artifactCount)
	assertMaxSizedItemsFitOrFailTyped(t)

	_, clientManager := startDaemonMeshPair(t, server, client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	projections, err := client.RemoteSessionsList(ctx, "server")
	if err != nil || len(projections) != sessionCount {
		t.Fatalf("paginated sessions = %d err=%v", len(projections), err)
	}
	artifacts, err := client.RemoteArtifactsList(ctx, "server", meshstate.ArtifactDocument)
	if err != nil || len(artifacts) != artifactCount {
		t.Fatalf("paginated artifacts = %d err=%v", len(artifacts), err)
	}
	var invalid meshSessionsListResponse
	err = clientManager.Call(ctx, "server", meshMethodSessionsList, meshPageRequest{Limit: meshPageItemMax + 1}, &invalid)
	if !isMeshInvalidRequest(err) {
		t.Fatalf("oversized page limit error = %v", err)
	}
	err = clientManager.Call(ctx, "server", meshMethodSessionsList, meshPageRequest{Cursor: "s.bad.cursor"}, &invalid)
	if !isMeshInvalidRequest(err) {
		t.Fatalf("invalid cursor error = %v", err)
	}
	var invalidArtifacts meshArtifactsListResponse
	err = clientManager.Call(ctx, "server", meshMethodArtifactsList, meshArtifactsListRequest{
		meshPageRequest: meshPageRequest{Limit: meshPageItemMax + 1},
	}, &invalidArtifacts)
	if !isMeshInvalidRequest(err) {
		t.Fatalf("oversized artifact page limit error = %v", err)
	}
	err = clientManager.Call(ctx, "server", meshMethodArtifactsList, meshArtifactsListRequest{
		meshPageRequest: meshPageRequest{Cursor: "a.bad.cursor"},
	}, &invalidArtifacts)
	if !isMeshInvalidRequest(err) {
		t.Fatalf("invalid artifact cursor error = %v", err)
	}

	firstArtifact, err := server.localMeshArtifactsPage("wrong-kind", meshArtifactsListRequest{
		Kind: meshstate.ArtifactDocument, meshPageRequest: meshPageRequest{Limit: 1},
	})
	if err != nil || firstArtifact.NextCursor == "" {
		t.Fatalf("create wrong-kind cursor: response=%#v err=%v", firstArtifact, err)
	}
	if _, err := server.localMeshArtifactsPage("wrong-kind", meshArtifactsListRequest{
		Kind: meshstate.ArtifactPullRequest, meshPageRequest: meshPageRequest{Cursor: firstArtifact.NextCursor},
	}); err == nil {
		t.Fatal("artifact cursor was accepted for a different kind")
	}

	firstSession, err := server.localMeshSessionsPage("expired", meshPageRequest{Limit: 1})
	if err != nil || firstSession.NextCursor == "" {
		t.Fatalf("create expiring cursor: response=%#v err=%v", firstSession, err)
	}
	revision, _, err := parseMeshCursor("s", firstSession.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	server.meshMu.RLock()
	app := server.meshApp
	server.meshMu.RUnlock()
	app.pagesMu.Lock()
	delete(app.sessionPages, meshPageKey("expired", revision))
	app.pagesMu.Unlock()
	if _, err := server.localMeshSessionsPage("expired", meshPageRequest{Cursor: firstSession.NextCursor}); err == nil {
		t.Fatal("expired session cursor was accepted")
	}
}

func assertSessionPagesBounded(t *testing.T, server *Daemon, peer string, want int) {
	t.Helper()
	cursor := ""
	total := 0
	for pageNumber := 0; pageNumber < 4096; pageNumber++ {
		page, err := server.localMeshSessionsPage(peer, meshPageRequest{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		assertMeshResultBounded(t, page)
		total += len(page.Sessions)
		cursor = page.NextCursor
		if cursor == "" {
			if total != want {
				t.Fatalf("session pages returned %d, want %d", total, want)
			}
			return
		}
	}
	t.Fatal("session pages did not terminate")
}

func assertArtifactPagesBounded(t *testing.T, server *Daemon, peer string, want int) {
	t.Helper()
	cursor := ""
	total := 0
	for pageNumber := 0; pageNumber < 4096; pageNumber++ {
		page, err := server.localMeshArtifactsPage(peer, meshArtifactsListRequest{meshPageRequest: meshPageRequest{Cursor: cursor}})
		if err != nil {
			t.Fatal(err)
		}
		assertMeshResultBounded(t, page)
		total += len(page.Artifacts)
		cursor = page.NextCursor
		if cursor == "" {
			if total != want {
				t.Fatalf("artifact pages returned %d, want %d", total, want)
			}
			return
		}
	}
	t.Fatal("artifact pages did not terminate")
}

func assertMeshResultBounded(t *testing.T, value any) {
	t.Helper()
	result, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(struct {
		Result json.RawMessage `json:"result,omitempty"`
	}{Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > meshPageResultBudget || len(payload) >= arcmuxmesh.MaxApplicationPayload {
		t.Fatalf("encoded result is %d bytes; budget=%d transport=%d", len(payload), meshPageResultBudget, arcmuxmesh.MaxApplicationPayload)
	}
}

func assertMaxSizedItemsFitOrFailTyped(t *testing.T) {
	t.Helper()
	now := time.Now().UTC()
	maxSummary := sessionview.Summary{
		Locator: sessionview.Locator{Version: sessionview.LocatorVersion, ProfileScope: sessionview.RootProfileScope, SessionID: strings.Repeat("s", 255)},
		Name:    strings.Repeat("n", meshSessionNameRunes), Agent: strings.Repeat("a", meshSessionFieldRunes),
		Transport: strings.Repeat("t", meshSessionFieldRunes), LaunchCWD: "~/" + strings.Repeat("c", meshSessionCWDRunes-2),
		OwnerID: strings.Repeat("o", meshSessionOwnerRunes), State: "idle", Health: strings.Repeat("h", meshSessionFieldRunes),
		StartedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Minute),
		Freshness: sessionview.Freshness{ObservedAt: now, SourceUpdatedAt: now.Add(-time.Minute)},
	}
	clean, err := meshAcceptSummary(maxSummary)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := buildSessionPage(meshSessionsListResponse{
		SourceEpoch: "boot-test", SourceRevision: 1, ProfileScopes: []sessionview.ProfileScope{sessionview.RootProfileScope}, Sessions: []sessionview.Summary{clean},
	}, 0, meshPageItemMax)
	if err != nil {
		t.Fatalf("max valid session item did not fit: %v", err)
	}
	assertMeshResultBounded(t, page)

	oversized := clean
	oversized.Name = strings.Repeat("x", meshPageResultBudget)
	_, _, err = buildSessionPage(meshSessionsListResponse{
		SourceEpoch: "boot-test", SourceRevision: 2, ProfileScopes: []sessionview.ProfileScope{sessionview.RootProfileScope}, Sessions: []sessionview.Summary{oversized},
	}, 0, meshPageItemMax)
	var typed meshPageTooLargeError
	if !errors.As(err, &typed) {
		t.Fatalf("oversized single item error = %v, want meshPageTooLargeError", err)
	}
}

func TestMeshWireAllowlistRemovesArbitrarySecretsAndTerminalControls(t *testing.T) {
	secret := "quasar-marmot-7vQ3pL9-never-pattern-matched"
	now := time.Now().UTC()
	summary := sessionview.Summary{
		Locator: sessionview.Locator{Version: sessionview.LocatorVersion, ProfileScope: sessionview.RootProfileScope, SessionID: "s-safe"},
		Name:    "worker\x1b[31m\u009bred", Agent: "codex\u0085", State: "idle\x7f",
		StartedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Minute),
		Freshness: sessionview.Freshness{ObservedAt: now, SourceUpdatedAt: now.Add(-time.Minute)},
		Work:      &sessionview.WorkSummary{Goal: secret, OverallGoal: secret, Source: secret, UpdatedAt: now},
		CurrentWork: &sessionview.CurrentWorkSummary{
			Summary: secret, Provenance: "spoofed.source", UpdatedAt: now,
		},
		History: &sessionview.HistoryReference{Basename: secret + ".md", Provenance: secret, UpdatedAt: now},
	}
	if _, err := meshAcceptSummary(summary); err == nil {
		t.Fatal("spoofed current-work provenance crossed session wire allowlist")
	}
	summary.CurrentWork = nil
	accepted, err := meshAcceptSummary(summary)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(accepted)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Work != nil || accepted.History != nil || strings.Contains(string(encoded), secret) {
		t.Fatalf("arbitrary secret crossed session wire allowlist: %s", encoded)
	}
	for _, control := range []string{"\x1b", "\x7f", "\u0085", "\u009b"} {
		if strings.Contains(string(encoded), control) {
			t.Fatalf("terminal control %q crossed session wire allowlist: %q", control, encoded)
		}
	}

	observed := now.Add(-time.Minute)
	artifact := meshstate.ArtifactEnvelope{
		SchemaVersion: meshstate.SchemaVersion, ID: "doc-safe", Kind: meshstate.ArtifactDocument,
		Title: secret, State: secret, URL: "https://example.com/" + secret + "?value=" + secret, PathHint: "~/" + secret,
		Repo:       &meshstate.RepoRef{Repo: "lin-labs/arcmux", Ref: secret, Commit: "abcdef1234567"},
		Provenance: secret, Revision: secret, RemoteObservedAt: &observed,
		ReceivedAt: now, FreshnessChangedAt: now, Freshness: meshstate.FreshnessFresh,
	}
	reference, err := meshArtifactReferenceFromEnvelope(artifact)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(reference)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("free-form artifact metadata crossed reference DTO: %s", encoded)
	}
	for _, forbidden := range []string{`"title"`, `"state"`, `"path_hint"`, `"provenance"`, `"revision"`, `"ref"`, `"content"`, `"remote_observed_at"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("forbidden artifact field %s crossed reference DTO: %s", forbidden, encoded)
		}
	}
	if strings.Contains(string(encoded), `"url"`) {
		t.Fatalf("caller-controlled artifact URL crossed reference DTO: %s", encoded)
	}

	future := summary
	future.StartedAt = now.Add(48 * time.Hour)
	if _, err := meshAcceptSummary(future); err == nil {
		t.Fatal("remote future timestamp was accepted")
	}
}

func TestMeshWireCarriesOnlyProvenSummarizedCurrentWork(t *testing.T) {
	now := time.Now().UTC()
	summary := sessionview.Summary{
		Locator: sessionview.Locator{Version: sessionview.LocatorVersion, ProfileScope: sessionview.RootProfileScope, SessionID: "s-work"},
		Agent:   "codex", State: "working", StartedAt: now.Add(-time.Hour), LastActivityAt: now,
		Freshness: sessionview.Freshness{ObservedAt: now, SourceUpdatedAt: now},
		CurrentWork: &sessionview.CurrentWorkSummary{
			Summary:    "  Ship\nremote surfaces password=hunter2  ",
			Provenance: hooks.OverallGoalSummarizerProvenance,
			UpdatedAt:  now,
		},
	}
	accepted, err := meshAcceptSummary(summary)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.CurrentWork == nil || accepted.CurrentWork.Summary != "Ship remote surfaces password=[REDACTED]" {
		t.Fatalf("current work=%#v", accepted.CurrentWork)
	}
	encoded, err := json.Marshal(accepted)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"hunter2", `"work"`, `"history"`, `"last_user_message"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("forbidden %q crossed wire: %s", forbidden, encoded)
		}
	}
}

func TestRemovedProfileCommitsEmptySnapshotAndMarksSessionsGone(t *testing.T) {
	d := newMeshApplicationTestDaemon(t, "ref")
	oldScope := sessionview.ProfileScope("profile:old")
	now := time.Now().UTC()
	first := meshSessionsListResponse{
		SourceEpoch: "boot-test", SourceRevision: 1,
		ProfileScopes: []sessionview.ProfileScope{sessionview.RootProfileScope, oldScope},
		Sessions: []sessionview.Summary{{
			Locator: sessionview.Locator{Version: sessionview.LocatorVersion, ProfileScope: oldScope, SessionID: "s-old"},
			Agent:   "codex", State: "idle", StartedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Minute),
			Freshness: sessionview.Freshness{ObservedAt: now, SourceUpdatedAt: now.Add(-time.Minute)},
		}},
	}
	if err := d.commitRemoteSessions("devbox", first); err != nil {
		t.Fatal(err)
	}
	second := meshSessionsListResponse{
		SourceEpoch: "boot-test", SourceRevision: 2,
		ProfileScopes: []sessionview.ProfileScope{sessionview.RootProfileScope},
	}
	if err := d.commitRemoteSessions("devbox", second); err != nil {
		t.Fatal(err)
	}
	store, _ := d.meshStateStore()
	projection, err := store.GetRemoteSession(meshstate.RemoteSessionLocator{
		SchemaVersion: meshstate.SchemaVersion, DeviceID: "devbox",
		ProfileScope: meshstate.ProfileScope(oldScope), SessionID: "s-old",
	})
	if err != nil || projection.Freshness != meshstate.FreshnessGone || projection.SourceRevision != 2 {
		t.Fatalf("removed profile projection = %#v err=%v", projection, err)
	}
}

func TestPeriodicReconcileAndRuntimeReconnectRestoreSubscription(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	client.meshMu.RLock()
	clientApp := client.meshApp
	client.meshMu.RUnlock()
	clientApp.runtimeMu.Lock()
	clientApp.reconcileInterval = 40 * time.Millisecond
	clientApp.runtimeMu.Unlock()
	_, clientManager := startDaemonMeshPair(t, server, client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.SubscribeMeshPeer(ctx, "server", []string{meshTopicSessions}); err != nil {
		t.Fatal(err)
	}

	managed := session.NewSession("s-periodic", "periodic", "codex", t.TempDir())
	managed.SetState(session.StateIdle)
	server.mu.Lock()
	server.sessions[managed.Snapshot().ID] = managed // deliberately no event
	server.mu.Unlock()
	clientStore, _ := client.meshStateStore()
	waitRemoteSessionFresh(t, clientStore, "server", "s-periodic")

	server.clearPeerSubscription("client") // simulate connection replacement's default-off state
	clientApp.stopRuntime()
	client.startMeshApplicationRuntime(clientManager)
	waitInboundSubscription(t, server, "client", meshTopicSessions)
}

func TestMeshSyncRerunsWhenEventAndReconnectArriveDuringPass(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	managed := session.NewSession("s-rerun", "rerun", "codex", t.TempDir())
	managed.SetState(session.StateIdle)
	server.mu.Lock()
	server.sessions[managed.Snapshot().ID] = managed
	server.mu.Unlock()
	_, clientManager := startDaemonMeshPair(t, server, client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.SubscribeMeshPeer(ctx, "server", []string{meshTopicSessions}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SyncMeshSessions(ctx, "server"); err != nil {
		t.Fatal(err)
	}
	store, _ := client.meshStateStore()
	waitProjectionFreshness(t, store, "server", "s-rerun", meshstate.FreshnessFresh)
	if err := store.MarkPeerDisconnected("server"); err != nil {
		t.Fatal(err)
	}
	waitProjectionFreshness(t, store, "server", "s-rerun", meshstate.FreshnessStale)

	client.meshMu.RLock()
	app := client.meshApp
	client.meshMu.RUnlock()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var first atomic.Bool
	app.runtimeMu.Lock()
	app.beforeSync = func(string) {
		entered <- struct{}{}
		if first.CompareAndSwap(false, true) {
			<-release
		}
	}
	app.runtimeMu.Unlock()
	defer func() {
		app.runtimeMu.Lock()
		app.beforeSync = nil
		app.runtimeMu.Unlock()
	}()

	server.clearPeerSubscription("client")
	client.scheduleMeshSync("server", false)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first sync did not reach deterministic gate")
	}
	// An event and a new connection observation both arrive while the first
	// inventory is blocked. Neither the dirty rerun nor resubscribe may drop.
	client.handleRemoteMeshEvent(arcmuxmesh.PeerEvent{PeerID: "server", Event: arcmuxmesh.Event{Name: "events.gap"}})
	connectedAt, ok := meshConnectedAt(clientManager, "server")
	if !ok {
		t.Fatal("server is not connected")
	}
	client.reconcileMeshStatuses(clientManager, map[string]time.Time{"server": connectedAt.Add(-time.Second)})
	waitProjectionFreshness(t, store, "server", "s-rerun", meshstate.FreshnessSyncing)
	close(release)
	select {
	case <-entered: // dirty event forced the second complete pass
	case <-ctx.Done():
		t.Fatal("dirty sync intent did not rerun")
	}
	waitMeshSyncIdle(t, app, "server")
	waitProjectionFreshness(t, store, "server", "s-rerun", meshstate.FreshnessFresh)
	waitInboundSubscription(t, server, "client", meshTopicSessions)

	events, cancelEvents := clientManager.SubscribeEvents(4)
	defer cancelEvents()
	server.emitStateChanged("s-rerun", session.StateWorking, "not-on-wire")
	select {
	case event := <-events:
		if event.Event.Name != "sessions.changed" {
			t.Fatalf("resumed event = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("events did not resume after reconnect restore")
	}
}

func TestMeshOneTopicSyncPreservesOtherInventoryFreshness(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	managed := session.NewSession("s-isolated", "isolated", "codex", t.TempDir())
	managed.SetState(session.StateIdle)
	server.mu.Lock()
	server.sessions[managed.Snapshot().ID] = managed
	server.mu.Unlock()
	serverStore, _ := server.meshStateStore()
	if err := serverStore.PutArtifact(meshstate.ArtifactEnvelope{
		ID: "goal-isolated", Kind: meshstate.ArtifactGoal, URL: "https://example.com/goal", Provenance: "test",
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = startDaemonMeshPair(t, server, client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.SyncMeshPeer(ctx, "server"); err != nil {
		t.Fatal(err)
	}
	store, _ := client.meshStateStore()
	binding := meshstate.SurfaceBinding{
		BindingID: "binding-isolated", LocalDeviceID: "client", Mux: "cmux",
		SurfaceID: "11111111-1111-4111-8111-111111111111", WorkspaceID: "22222222-2222-4222-8222-222222222222",
		Locator: meshstate.RemoteSessionLocator{
			SchemaVersion: meshstate.SchemaVersion, DeviceID: "server",
			ProfileScope: meshstate.RootProfileScope, SessionID: "s-isolated",
		},
		Source: "human",
	}
	if err := store.PutSurfaceBinding(binding, false); err != nil {
		t.Fatal(err)
	}
	assertInventoryFreshness(t, store, "server", meshstate.FreshnessFresh, meshstate.FreshnessFresh)
	assertResolvedFreshness(t, store, binding.SurfaceID, meshstate.FreshnessFresh)

	client.meshMu.RLock()
	app := client.meshApp
	client.meshMu.RUnlock()
	runBlocked := func(run func() error, during func()) {
		t.Helper()
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		app.runtimeMu.Lock()
		app.beforeSync = func(string) {
			entered <- struct{}{}
			<-release
		}
		app.runtimeMu.Unlock()
		done := make(chan error, 1)
		go func() { done <- run() }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("one-topic sync did not reach fetch gate")
		}
		during()
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		app.runtimeMu.Lock()
		app.beforeSync = nil
		app.runtimeMu.Unlock()
	}

	runBlocked(func() error {
		_, err := client.SyncMeshSessions(ctx, "server")
		return err
	}, func() {
		assertInventoryFreshness(t, store, "server", meshstate.FreshnessSyncing, meshstate.FreshnessFresh)
		artifacts, err := store.ListArtifactsForOrigin("server", meshstate.ArtifactGoal)
		if err != nil || len(artifacts) != 1 || artifacts[0].Freshness != meshstate.FreshnessFresh {
			t.Fatalf("session-only sync perturbed cached artifacts: artifacts=%#v err=%v", artifacts, err)
		}
		assertResolvedFreshness(t, store, binding.SurfaceID, meshstate.FreshnessSyncing)
	})
	assertInventoryFreshness(t, store, "server", meshstate.FreshnessFresh, meshstate.FreshnessFresh)

	runBlocked(func() error {
		_, err := client.SyncMeshArtifacts(ctx, "server")
		return err
	}, func() {
		assertInventoryFreshness(t, store, "server", meshstate.FreshnessFresh, meshstate.FreshnessSyncing)
		projections, err := store.ListRemoteSessions("server", meshstate.RootProfileScope)
		if err != nil || len(projections) != 1 || projections[0].Freshness != meshstate.FreshnessFresh {
			t.Fatalf("artifact-only sync perturbed cached sessions: projections=%#v err=%v", projections, err)
		}
		assertResolvedFreshness(t, store, binding.SurfaceID, meshstate.FreshnessFresh)
	})
	assertInventoryFreshness(t, store, "server", meshstate.FreshnessFresh, meshstate.FreshnessFresh)

	// A combined sync marks both inventories before the alphabetically first
	// artifact fetch, so aggregate/resolved freshness cannot flash fresh between
	// the two commits.
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	app.runtimeMu.Lock()
	app.beforeSync = func(string) { entered <- struct{}{}; <-release }
	app.runtimeMu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := client.SyncMeshPeer(ctx, "server")
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("combined sync did not reach first topic gate")
	}
	assertInventoryFreshness(t, store, "server", meshstate.FreshnessSyncing, meshstate.FreshnessSyncing)
	state, err := store.GetPeer("server")
	if err != nil || state.Freshness != meshstate.FreshnessSyncing {
		t.Fatalf("combined sync aggregate freshness = %#v err=%v", state, err)
	}
	assertResolvedFreshness(t, store, binding.SurfaceID, meshstate.FreshnessSyncing)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	app.runtimeMu.Lock()
	app.beforeSync = nil
	app.runtimeMu.Unlock()
	assertInventoryFreshness(t, store, "server", meshstate.FreshnessFresh, meshstate.FreshnessFresh)
}

func TestMeshTransportStopWaitsForInFlightSyncWorker(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	client := newMeshApplicationTestDaemon(t, "client")
	managed := session.NewSession("s-stop", "stop", "codex", t.TempDir())
	managed.SetState(session.StateIdle)
	server.mu.Lock()
	server.sessions[managed.Snapshot().ID] = managed
	server.mu.Unlock()
	_, _ = startDaemonMeshPair(t, server, client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.SubscribeMeshPeer(ctx, "server", []string{meshTopicSessions}); err != nil {
		t.Fatal(err)
	}

	client.meshMu.RLock()
	app := client.meshApp
	client.meshMu.RUnlock()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	app.runtimeMu.Lock()
	app.beforeSync = func(string) {
		entered <- struct{}{}
		<-release
	}
	app.runtimeMu.Unlock()
	defer func() {
		app.runtimeMu.Lock()
		app.beforeSync = nil
		app.runtimeMu.Unlock()
	}()

	client.scheduleMeshSync("server", false)
	select {
	case <-entered:
	case <-ctx.Done():
		close(release)
		t.Fatal("sync worker did not reach deterministic gate")
	}
	stopped := make(chan struct{})
	go func() {
		client.stopMeshTransport()
		close(stopped)
	}()
	select {
	case <-stopped:
		close(release)
		waitMeshSyncIdle(t, app, "server")
		t.Fatal("mesh transport stop returned while sync worker was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("mesh transport stop did not finish after sync worker exited")
	}
}

func TestMeshDesiredSubscriptionSurvivesDaemonRestart(t *testing.T) {
	server := newMeshApplicationTestDaemon(t, "server")
	managed := session.NewSession("s-restart", "restart", "codex", t.TempDir())
	managed.SetState(session.StateIdle)
	server.mu.Lock()
	server.sessions[managed.Snapshot().ID] = managed
	server.mu.Unlock()
	token, err := arcmuxmesh.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	serverRegistry := &arcmuxmesh.Registry{
		Version: 1, DeviceID: "server", Serve: true,
		Accept: map[string]string{"client": arcmuxmesh.TokenHash(token)},
		Grants: map[string][]string{"client": {arcmuxmesh.ScopeSessionsRead, arcmuxmesh.ScopeEventsRead}},
	}
	serverManager := arcmuxmesh.New(meshApplicationTestConfig("127.0.0.1:0"), serverRegistry, testDiscardLogger())
	if err := server.registerMeshApplication(serverManager); err != nil {
		t.Fatal(err)
	}
	meshCtx, cancelMesh := context.WithCancel(context.Background())
	defer cancelMesh()
	if err := serverManager.Start(meshCtx); err != nil {
		t.Fatal(err)
	}
	server.meshMu.Lock()
	server.mesh = serverManager
	server.meshMu.Unlock()
	server.startMeshApplicationRuntime(serverManager)

	clientRoot := t.TempDir()
	newClient := func() *Daemon { return newMeshApplicationTestDaemonAtRoot(t, "client", clientRoot) }
	newClientManager := func(client *Daemon) *arcmuxmesh.Manager {
		registry := &arcmuxmesh.Registry{
			Version: 1, DeviceID: "client", Accept: map[string]string{},
			Peers:  []arcmuxmesh.Peer{{ID: "server", URL: "ws://" + serverManager.Addr() + "/v1/mesh", Token: token}},
			Grants: map[string][]string{"server": {arcmuxmesh.ScopeSessionsRead, arcmuxmesh.ScopeEventsRead}},
		}
		manager := arcmuxmesh.New(meshApplicationTestConfig("127.0.0.1:0"), registry, testDiscardLogger())
		if err := client.registerMeshApplication(manager); err != nil {
			t.Fatal(err)
		}
		if err := manager.Start(meshCtx); err != nil {
			t.Fatal(err)
		}
		waitDaemonMeshState(t, manager, "server", "connected")
		client.meshMu.Lock()
		client.mesh = manager
		client.meshMu.Unlock()
		client.startMeshApplicationRuntime(manager)
		return manager
	}

	client1 := newClient()
	_ = newClientManager(client1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client1.SubscribeMeshPeer(ctx, "server", []string{meshTopicSessions}); err != nil {
		t.Fatal(err)
	}
	if _, err := client1.SyncMeshSessions(ctx, "server"); err != nil {
		t.Fatal(err)
	}
	store1, _ := client1.meshStateStore()
	waitProjectionFreshness(t, store1, "server", "s-restart", meshstate.FreshnessFresh)
	if topics, err := store1.DesiredTopics("server"); err != nil || fmt.Sprint(topics) != "[sessions]" {
		t.Fatalf("persisted topics before restart = %v err=%v", topics, err)
	}
	client1.stopMeshTransport()
	server.clearPeerSubscription("client")

	client2 := newClient()
	store2, _ := client2.meshStateStore()
	projections, err := store2.ListRemoteSessions("server", meshstate.RootProfileScope)
	if err != nil || len(projections) != 1 || projections[0].Locator.SessionID != "s-restart" {
		t.Fatalf("persisted projection after restart = %#v err=%v", projections, err)
	}
	if topics := client2.desiredMeshTopics("server"); fmt.Sprint(topics) != "[sessions]" {
		t.Fatalf("restored in-memory desired topics = %v", topics)
	}
	clientManager2 := newClientManager(client2)
	waitInboundSubscription(t, server, "client", meshTopicSessions)
	waitProjectionFreshness(t, store2, "server", "s-restart", meshstate.FreshnessFresh)

	events, cancelEvents := clientManager2.SubscribeEvents(4)
	defer cancelEvents()
	server.emitStateChanged("s-restart", session.StateWorking, "not-on-wire")
	select {
	case event := <-events:
		if event.Event.Name != "sessions.changed" {
			t.Fatalf("post-restart event = %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("events did not resume after daemon restart")
	}

	client2.stopMeshTransport()
	server.stopMeshTransport()
}

func isMeshInvalidRequest(err error) bool {
	var rpcErr *arcmuxmesh.RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == arcmuxmesh.ErrorInvalidRequest
}

func TestMeshProtocolRootDerivesFromSessionStateDirectory(t *testing.T) {
	root := t.TempDir()
	d := New(&config.Config{
		Daemon: config.DaemonConfig{Socket: filepath.Join(root, "daemon.sock")},
		Mux:    config.MuxConfig{Backend: "tmux"}, Tmux: config.TmuxConfig{SocketName: "mesh-root-test"},
		Hooks:  config.HooksConfig{SessionStateDir: filepath.Join(root, "mux", "sessions"), HookOutputDir: filepath.Join(root, "hooks")},
		Agents: config.DefaultAgentProfiles(),
	}, testDiscardLogger())
	if err := d.initMeshApplication(); err != nil {
		t.Fatal(err)
	}
	store, _ := d.meshStateStore()
	if got, want := store.Root(), filepath.Join(root, "mux"); got != want {
		t.Fatalf("mesh protocol root = %q, want %q", got, want)
	}
}

func TestMeshEventTopicsRequireCorrespondingDataScopes(t *testing.T) {
	tests := []struct {
		name         string
		grants       []string
		allowedTopic string
		deniedTopic  string
	}{
		{
			name:        "events only cannot read sessions",
			grants:      []string{arcmuxmesh.ScopeEventsRead},
			deniedTopic: meshTopicSessions,
		},
		{
			name:        "events only cannot read artifacts",
			grants:      []string{arcmuxmesh.ScopeEventsRead},
			deniedTopic: meshTopicArtifacts,
		},
		{
			name:         "session scope does not escalate to artifacts",
			grants:       []string{arcmuxmesh.ScopeEventsRead, arcmuxmesh.ScopeSessionsRead},
			allowedTopic: meshTopicSessions,
			deniedTopic:  meshTopicArtifacts,
		},
		{
			name:         "artifact scope does not escalate to sessions",
			grants:       []string{arcmuxmesh.ScopeEventsRead, arcmuxmesh.ScopeArtifactsRead},
			allowedTopic: meshTopicArtifacts,
			deniedTopic:  meshTopicSessions,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newMeshApplicationTestDaemon(t, "server")
			client := newMeshApplicationTestDaemon(t, "client")
			_, _ = startDaemonMeshPairWithScopes(t, server, client, test.grants)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if test.allowedTopic != "" {
				if topics, err := client.SubscribeMeshPeer(ctx, "server", []string{test.allowedTopic}); err != nil || len(topics) != 1 || topics[0] != test.allowedTopic {
					t.Fatalf("allowed subscription = %v, %v", topics, err)
				}
				switch test.allowedTopic {
				case meshTopicSessions:
					if _, err := client.SyncMeshSessions(ctx, "server"); err != nil {
						t.Fatalf("session-only sync: %v", err)
					}
					if _, err := client.SyncMeshArtifacts(ctx, "server"); !isMeshPermissionDenied(err) {
						t.Fatalf("artifact sync with session-only grant = %v, want permission_denied", err)
					}
					h := NewHTTPServer(client, "127.0.0.1:0")
					if response := meshHTTPRequest(h, http.MethodPost, "/mesh/sessions/sync?peer=server", nil); response.Code != http.StatusOK {
						t.Fatalf("session-only HTTP sync status=%d body=%s", response.Code, response.Body.String())
					}
					if response := meshHTTPRequest(h, http.MethodPost, "/mesh/artifacts/sync?peer=server", nil); response.Code != http.StatusServiceUnavailable {
						t.Fatalf("artifact HTTP sync with session-only grant status=%d body=%s", response.Code, response.Body.String())
					}
				case meshTopicArtifacts:
					if _, err := client.SyncMeshArtifacts(ctx, "server"); err != nil {
						t.Fatalf("artifact-only sync: %v", err)
					}
					if _, err := client.SyncMeshSessions(ctx, "server"); !isMeshPermissionDenied(err) {
						t.Fatalf("session sync with artifact-only grant = %v, want permission_denied", err)
					}
					h := NewHTTPServer(client, "127.0.0.1:0")
					if response := meshHTTPRequest(h, http.MethodPost, "/mesh/artifacts/sync?peer=server", nil); response.Code != http.StatusOK {
						t.Fatalf("artifact-only HTTP sync status=%d body=%s", response.Code, response.Body.String())
					}
					if response := meshHTTPRequest(h, http.MethodPost, "/mesh/sessions/sync?peer=server", nil); response.Code != http.StatusServiceUnavailable {
						t.Fatalf("session HTTP sync with artifact-only grant status=%d body=%s", response.Code, response.Body.String())
					}
				}
			}
			if _, err := client.SubscribeMeshPeer(ctx, "server", []string{test.deniedTopic}); !isMeshPermissionDenied(err) {
				t.Fatalf("denied subscription error = %v, want permission_denied", err)
			}
			if test.allowedTopic != "" {
				if _, err := client.SubscribeMeshPeer(ctx, "server", []string{test.allowedTopic, test.deniedTopic}); !isMeshPermissionDenied(err) {
					t.Fatalf("mixed subscription error = %v, want permission_denied", err)
				}
				server.meshMu.RLock()
				app := server.meshApp
				server.meshMu.RUnlock()
				app.subsMu.RLock()
				stored := app.subs["client"]
				app.subsMu.RUnlock()
				if len(stored.topics) != 1 || !stored.topics[test.allowedTopic] || stored.topics[test.deniedTopic] {
					t.Fatalf("failed mixed request changed authorized topics: %#v", stored.topics)
				}
			}
		})
	}
}

func isMeshPermissionDenied(err error) bool {
	var rpcErr *arcmuxmesh.RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == arcmuxmesh.ErrorPermissionDenied
}

func newMeshApplicationTestDaemon(t *testing.T, deviceID string) *Daemon {
	t.Helper()
	return newMeshApplicationTestDaemonAtRoot(t, deviceID, t.TempDir())
}

func newMeshApplicationTestDaemonAtRoot(t *testing.T, deviceID, root string) *Daemon {
	t.Helper()
	d := New(&config.Config{
		Daemon: config.DaemonConfig{Socket: filepath.Join(root, "daemon.sock"), LogDir: filepath.Join(root, "logs")},
		Mux:    config.MuxConfig{Backend: "tmux"}, Tmux: config.TmuxConfig{SocketName: "mesh-app-" + deviceID},
		Hooks: config.HooksConfig{
			HookOutputDir: filepath.Join(root, "hooks"), SessionStateDir: filepath.Join(root, "mux", "sessions"),
		},
		Agents: config.DefaultAgentProfiles(),
	}, testDiscardLogger())
	d.ctx = context.Background()
	if err := d.initMeshApplication(); err != nil {
		t.Fatal(err)
	}
	d.setMeshDeviceID(deviceID)
	return d
}

func startDaemonMeshPair(t *testing.T, server, client *Daemon) (*arcmuxmesh.Manager, *arcmuxmesh.Manager) {
	t.Helper()
	allScopes := []string{arcmuxmesh.ScopeSessionsRead, arcmuxmesh.ScopeArtifactsRead, arcmuxmesh.ScopeEventsRead}
	return startDaemonMeshPairWithScopes(t, server, client, allScopes)
}

func startDaemonMeshPairWithScopes(t *testing.T, server, client *Daemon, serverGrants []string) (*arcmuxmesh.Manager, *arcmuxmesh.Manager) {
	t.Helper()
	token, err := arcmuxmesh.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	serverRegistry := &arcmuxmesh.Registry{
		Version: 1, DeviceID: "server", Serve: true,
		Accept: map[string]string{"client": arcmuxmesh.TokenHash(token)},
		Grants: map[string][]string{"client": append([]string(nil), serverGrants...)},
	}
	serverManager := arcmuxmesh.New(meshApplicationTestConfig("127.0.0.1:0"), serverRegistry, testDiscardLogger())
	if err := server.registerMeshApplication(serverManager); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := serverManager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clientRegistry := &arcmuxmesh.Registry{
		Version: 1, DeviceID: "client", Accept: map[string]string{},
		Peers: []arcmuxmesh.Peer{{ID: "server", URL: "ws://" + serverManager.Addr() + "/v1/mesh", Token: token}},
		Grants: map[string][]string{"server": {
			arcmuxmesh.ScopeSessionsRead, arcmuxmesh.ScopeArtifactsRead, arcmuxmesh.ScopeEventsRead,
		}},
	}
	clientManager := arcmuxmesh.New(meshApplicationTestConfig("127.0.0.1:0"), clientRegistry, testDiscardLogger())
	if err := client.registerMeshApplication(clientManager); err != nil {
		t.Fatal(err)
	}
	if err := clientManager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitDaemonMeshState(t, serverManager, "client", "connected")
	waitDaemonMeshState(t, clientManager, "server", "connected")
	server.meshMu.Lock()
	server.mesh = serverManager
	server.meshMu.Unlock()
	client.meshMu.Lock()
	client.mesh = clientManager
	client.meshMu.Unlock()
	server.startMeshApplicationRuntime(serverManager)
	client.startMeshApplicationRuntime(clientManager)
	t.Cleanup(func() {
		cancel()
		client.stopMeshTransport()
		server.stopMeshTransport()
	})
	return serverManager, clientManager
}

func meshApplicationTestConfig(addr string) config.ParsedMeshConfig {
	return config.ParsedMeshConfig{
		Enabled: true, ListenAddr: addr, HeartbeatInterval: 25 * time.Millisecond,
		StaleAfter: 200 * time.Millisecond, DeadAfter: 400 * time.Millisecond,
		ReconnectMin: 10 * time.Millisecond, ReconnectMax: 50 * time.Millisecond,
		HandshakeTimeout: time.Second, MaxMessageBytes: 64 << 10, WriterQueue: 64,
	}
}

func waitDaemonMeshState(t *testing.T, manager *arcmuxmesh.Manager, peer, state string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, status := range manager.Status() {
			if status.PeerID == peer && status.State == state {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("peer %s did not reach %s: %#v", peer, state, manager.Status())
}

func waitMeshPeerFreshness(t *testing.T, store *meshstate.Store, peer string, freshness meshstate.Freshness) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if state, err := store.GetPeer(peer); err == nil && state.Freshness == freshness {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	state, err := store.GetPeer(peer)
	t.Fatalf("peer %s did not reach %s: state=%#v err=%v", peer, freshness, state, err)
}

func waitRemoteSessionFresh(t *testing.T, store *meshstate.Store, peer, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		projections, err := store.ListRemoteSessions(peer, "")
		if err == nil {
			for _, projection := range projections {
				if projection.Locator.SessionID == sessionID && projection.Freshness == meshstate.FreshnessFresh {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	projections, err := store.ListRemoteSessions(peer, "")
	t.Fatalf("remote session %s from %s did not become fresh: projections=%#v err=%v", sessionID, peer, projections, err)
}

func waitProjectionFreshness(t *testing.T, store *meshstate.Store, peer, sessionID string, freshness meshstate.Freshness) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		projections, err := store.ListRemoteSessions(peer, "")
		if err == nil {
			for _, projection := range projections {
				if projection.Locator.SessionID == sessionID && projection.Freshness == freshness {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	projections, err := store.ListRemoteSessions(peer, "")
	t.Fatalf("session %s from %s did not reach %s: projections=%#v err=%v", sessionID, peer, freshness, projections, err)
}

func waitMeshSyncIdle(t *testing.T, app *meshApplication, peer string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		app.syncMu.Lock()
		_, active := app.syncing[peer]
		app.syncMu.Unlock()
		if !active {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("mesh sync for %s did not become idle", peer)
}

func assertInventoryFreshness(t *testing.T, store *meshstate.Store, peer string, sessions, artifacts meshstate.Freshness) {
	t.Helper()
	state, err := store.GetPeer(peer)
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionFreshness != sessions || state.ArtifactFreshness != artifacts {
		t.Fatalf("peer %s inventory freshness session=%s artifact=%s, want %s/%s: %#v", peer, state.SessionFreshness, state.ArtifactFreshness, sessions, artifacts, state)
	}
}

func assertResolvedFreshness(t *testing.T, store *meshstate.Store, surfaceID string, freshness meshstate.Freshness) {
	t.Helper()
	resolved, err := store.ResolveSurfaceBinding(surfaceID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.EffectiveFreshness != freshness {
		t.Fatalf("resolved surface freshness=%s, want %s: %#v", resolved.EffectiveFreshness, freshness, resolved)
	}
}

func waitInboundSubscription(t *testing.T, d *Daemon, peer, topic string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d.meshMu.RLock()
		app := d.meshApp
		d.meshMu.RUnlock()
		if app != nil {
			app.subsMu.RLock()
			subscription, ok := app.subs[peer]
			active := ok && subscription.topics[topic]
			app.subsMu.RUnlock()
			if active {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("peer %s did not restore inbound %s subscription", peer, topic)
}

func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
