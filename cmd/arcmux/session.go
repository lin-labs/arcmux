package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	arcmuxv1 "github.com/lin-labs/arcmux/gen/arcmux/v1"
	"github.com/lin-labs/arcmux/internal/config"
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
type sessionCatalogFetcher func(string) (sessionCatalogList, error)

type sessionCatalogRecord struct {
	RemoteSessionKey string  `json:"remote_session_key"`
	ProfileScope     string  `json:"profile_scope"`
	SessionID        string  `json:"session_id"`
	SessionName      string  `json:"session_name"`
	Agent            string  `json:"agent"`
	State            string  `json:"state"`
	OwnerID          string  `json:"owner_id,omitempty"`
	Transport        string  `json:"transport,omitempty"`
	CWD              string  `json:"cwd,omitempty"`
	TmuxTarget       string  `json:"tmux_target,omitempty"`
	TmuxSessionName  string  `json:"tmux_session_name,omitempty"`
	StartedAt        string  `json:"started_at"`
	LastActivityAt   string  `json:"last_activity_at"`
	HistoryBasename  *string `json:"history_basename"`
}

type sessionCatalogList struct {
	ObservedAt string                 `json:"observed_at"`
	Sessions   []sessionCatalogRecord `json:"sessions"`
}

func cmdSession(args []string, stdout io.Writer) error {
	return cmdSessionWithDeps(
		args, stdout, os.Getenv, lookupSessionSelfCatalog, fetchSessionCatalog,
	)
}

func cmdSessionWithRuntime(args []string, stdout io.Writer, getenv func(string) string, catalog sessionSelfCatalogLookup) error {
	return cmdSessionWithDeps(args, stdout, getenv, catalog, nil)
}

func cmdSessionWithDeps(
	args []string,
	stdout io.Writer,
	getenv func(string) string,
	catalog sessionSelfCatalogLookup,
	fetchCatalog sessionCatalogFetcher,
) error {
	if len(args) == 0 {
		return errors.New("usage: arcmux session self --json | arcmux session list --json [--config path]")
	}
	switch args[0] {
	case "self":
		return cmdSessionSelf(args[1:], stdout, getenv, catalog)
	case "list":
		return cmdSessionList(args[1:], stdout, fetchCatalog)
	default:
		return errors.New("usage: arcmux session self --json | arcmux session list --json [--config path]")
	}
}

func cmdSessionSelf(args []string, stdout io.Writer, getenv func(string) string, catalog sessionSelfCatalogLookup) error {
	if catalog == nil {
		return errors.New("arcmux session catalog lookup is unavailable")
	}
	fs := flag.NewFlagSet("session self", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
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

func cmdSessionList(args []string, stdout io.Writer, fetch sessionCatalogFetcher) error {
	if fetch == nil {
		return errors.New("arcmux session export is unavailable")
	}
	fs := flag.NewFlagSet("session list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print JSON")
	configPath := fs.String("config", "", "config path")
	fs.StringVar(configPath, "c", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !*asJSON {
		return errors.New("usage: arcmux session list --json [--config path]")
	}
	catalog, err := fetch(*configPath)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(catalog)
}

func fetchSessionCatalog(configPath string) (sessionCatalogList, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return sessionCatalogList{}, fmt.Errorf("load config: %w", err)
	}
	if cfg.Daemon.HTTPAddr == "" {
		return sessionCatalogList{}, errors.New("daemon http_addr is disabled")
	}
	request, err := http.NewRequest(http.MethodGet, "http://"+cfg.Daemon.HTTPAddr+"/session-catalog", nil)
	if err != nil {
		return sessionCatalogList{}, fmt.Errorf("build request: %w", err)
	}
	if cfg.Daemon.HTTPAuthToken != "" {
		request.Header.Set("Authorization", "Bearer "+cfg.Daemon.HTTPAuthToken)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return sessionCatalogList{}, fmt.Errorf("fetch session catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		return sessionCatalogList{}, fmt.Errorf(
			"session catalog API %s: %s", response.Status, string(body),
		)
	}
	var payload sessionCatalogList
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return sessionCatalogList{}, fmt.Errorf("decode session catalog: %w", err)
	}
	return payload, nil
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
