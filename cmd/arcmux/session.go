package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/sessionview"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type sessionSelfIdentity struct {
	ProfileScope    sessionview.ProfileScope `json:"profile_scope"`
	SessionID       string                   `json:"session_id"`
	Agent           string                   `json:"agent"`
	CWD             string                   `json:"cwd"`
	HistoryBasename *string                  `json:"history_basename"`
	Source          string                   `json:"source"`
}

type sessionSelfCatalogRecord struct {
	ProfileScope    sessionview.ProfileScope
	SessionID       string
	Agent           string
	CWD             string
	HistoryBasename *string
}

type sessionSelfCatalogLookup func(string, string) (sessionSelfCatalogRecord, error)

func cmdSession(args []string, stdout io.Writer) error {
	return cmdSessionWithRuntime(args, stdout, os.Getenv, lookupSessionSelfCatalog)
}

func cmdSessionWithRuntime(args []string, stdout io.Writer, getenv func(string) string, catalog sessionSelfCatalogLookup) error {
	if len(args) == 0 || args[0] != "self" {
		return errors.New("usage: arcmux session self --json")
	}
	fs := flag.NewFlagSet("session self", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || !*asJSON {
		return errors.New("usage: arcmux session self --json")
	}
	identity, err := resolveSessionSelf(getenv, catalog)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(identity)
}

func resolveSessionSelf(getenv func(string) string, catalog sessionSelfCatalogLookup) (sessionSelfIdentity, error) {
	scope := sessionview.ProfileScope(getenv("ARCMUX_PROFILE_SCOPE"))
	id := getenv("ARCMUX_SESSION_ID")
	if _, err := sessionview.NewLocator(scope, id); err != nil {
		return sessionSelfIdentity{}, errors.New("supervised arcmux session identity is unavailable")
	}
	socket := getenv("ARCMUX_DAEMON_SOCKET")
	if socket == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return sessionSelfIdentity{}, errors.New("supervised arcmux daemon socket is unavailable")
	}
	if catalog == nil {
		return sessionSelfIdentity{}, errors.New("arcmux session catalog lookup is unavailable")
	}
	record, err := catalog(socket, id)
	if err != nil || record.ProfileScope != scope || record.SessionID != id || record.Agent == "" || record.CWD == "" {
		return sessionSelfIdentity{}, errors.New("session identity is not present in the exact arcmux daemon catalog")
	}
	return sessionSelfIdentity{
		ProfileScope: scope, SessionID: id, Agent: record.Agent, CWD: record.CWD,
		HistoryBasename: record.HistoryBasename, Source: "daemon_catalog",
	}, nil
}

func lookupSessionSelfCatalog(socketPath, sessionID string) (sessionSelfCatalogRecord, error) {
	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return sessionSelfCatalogRecord{}, fmt.Errorf("dial daemon catalog: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := arcmuxv1.NewAgentRuntimeClient(conn).ListSessions(ctx, &arcmuxv1.ListSessionsRequest{})
	if err != nil {
		return sessionSelfCatalogRecord{}, fmt.Errorf("list daemon sessions: %w", err)
	}
	var match *arcmuxv1.SessionSummary
	for _, candidate := range response.Sessions {
		if candidate.GetSessionId() != sessionID {
			continue
		}
		if match != nil {
			return sessionSelfCatalogRecord{}, errors.New("daemon catalog returned ambiguous session identity")
		}
		match = candidate
	}
	if match == nil {
		return sessionSelfCatalogRecord{}, errors.New("session not found")
	}
	var history *string
	if match.GetHistoryBasename() != "" {
		basename := match.GetHistoryBasename()
		history = &basename
	}
	return sessionSelfCatalogRecord{
		ProfileScope: sessionview.ProfileScope(match.GetProfileScope()), SessionID: match.GetSessionId(),
		Agent: match.GetAgent(), CWD: match.GetCwd(), HistoryBasename: history,
	}, nil
}
