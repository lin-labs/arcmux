package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lin-labs/arcmux/internal/config"
	"github.com/lin-labs/arcmux/internal/mesh"
	"github.com/lin-labs/arcmux/internal/meshstate"
)

func cmdMeshSessions(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: arcmux mesh sessions <peer> [--profile root|profile:<name>]")
	}
	peerID := rest[0]
	fs := flag.NewFlagSet("mesh sessions", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	profile := fs.String("profile", "", "profile scope filter")
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: arcmux mesh sessions <peer> [--profile root|profile:<name>]")
	}
	if _, err := meshAPI(cfg, http.MethodPost, "/mesh/sessions/sync?peer="+url.QueryEscape(peerID)); err != nil {
		return err
	}
	path := "/mesh/sessions?peer=" + url.QueryEscape(peerID)
	if *profile != "" {
		path += "&profile=" + url.QueryEscape(*profile)
	}
	return writeMeshJSON(cfg, http.MethodGet, path, nil, stdout)
}

func cmdMeshSession(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) != 3 {
		return errors.New("usage: arcmux mesh session <peer> <root|profile:name> <session-id>")
	}
	path := "/mesh/session?live=1&peer=" + url.QueryEscape(rest[0]) + "&profile=" + url.QueryEscape(rest[1]) + "&session=" + url.QueryEscape(rest[2])
	return writeMeshJSON(cfg, http.MethodGet, path, nil, stdout)
}

func cmdMeshRemoteArtifacts(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: arcmux mesh artifacts <peer> [--kind kind]")
	}
	peerID := rest[0]
	fs := flag.NewFlagSet("mesh artifacts", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	kind := fs.String("kind", "", "artifact kind")
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: arcmux mesh artifacts <peer> [--kind kind]")
	}
	if _, err := meshAPI(cfg, http.MethodPost, "/mesh/artifacts/sync?peer="+url.QueryEscape(peerID)); err != nil {
		return err
	}
	path := "/mesh/artifacts?peer=" + url.QueryEscape(peerID)
	if *kind != "" {
		path += "&kind=" + url.QueryEscape(*kind)
	}
	return writeMeshJSON(cfg, http.MethodGet, path, nil, stdout)
}

func cmdMeshRemoteArtifact(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) != 3 {
		return errors.New("usage: arcmux mesh artifact <peer> <kind> <source-id>")
	}
	path := "/mesh/artifact?live=1&peer=" + url.QueryEscape(rest[0]) +
		"&kind=" + url.QueryEscape(rest[1]) + "&id=" + url.QueryEscape(rest[2])
	return writeMeshJSON(cfg, http.MethodGet, path, nil, stdout)
}

func cmdMeshSubscribe(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("usage: arcmux mesh subscribe <peer> [sessions] [artifacts]")
	}
	topics := rest[1:]
	if len(topics) == 0 {
		topics = []string{"sessions", "artifacts"}
	}
	for _, topic := range topics {
		if topic != "sessions" && topic != "artifacts" {
			return fmt.Errorf("unsupported mesh subscription topic %q", topic)
		}
	}
	body, _ := json.Marshal(struct {
		PeerID string   `json:"peer_id"`
		Topics []string `json:"topics"`
	}{PeerID: rest[0], Topics: topics})
	return writeMeshJSON(cfg, http.MethodPut, "/mesh/subscribe", body, stdout)
}

func cmdArtifact(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: arcmux artifact record|list")
	}
	switch args[0] {
	case "record":
		return cmdArtifactRecord(args[1:], stdout)
	case "list":
		return cmdArtifactList(args[1:], stdout)
	default:
		return fmt.Errorf("unknown artifact subcommand %q", args[0])
	}
}

func cmdArtifactRecord(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("artifact record", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	kind := fs.String("kind", "", "goal, session_history, document, branch, commit, or pull_request")
	id := fs.String("id", "", "stable artifact id")
	title := fs.String("title", "", "display title")
	state := fs.String("state", "", "artifact state")
	artifactURL := fs.String("url", "", "sanitized HTTPS URL")
	pathHint := fs.String("path-hint", "", "home-relative ~/ path")
	repo := fs.String("repo", "", "owner/repository")
	ref := fs.String("ref", "", "branch or ref")
	commit := fs.String("commit", "", "commit SHA")
	revision := fs.String("revision", "", "source revision")
	provenance := fs.String("provenance", "local-cli", "source description")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 || *kind == "" || *id == "" {
		return errors.New("usage: arcmux artifact record --kind <kind> --id <id> [metadata flags]")
	}
	artifact := meshstate.ArtifactEnvelope{
		SchemaVersion: meshstate.SchemaVersion,
		ID:            *id,
		Kind:          meshstate.ArtifactKind(*kind),
		Title:         *title,
		State:         *state,
		URL:           *artifactURL,
		PathHint:      *pathHint,
		Provenance:    *provenance,
		Revision:      *revision,
		ReceivedAt:    time.Now().UTC(),
	}
	if *repo != "" {
		artifact.Repo = &meshstate.RepoRef{Repo: *repo, Ref: *ref, Commit: *commit}
	} else if *ref != "" || *commit != "" {
		return errors.New("--ref and --commit require --repo")
	}
	body, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	return writeMeshJSON(cfg, http.MethodPut, "/mesh/artifact", body, stdout)
}

func cmdArtifactList(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("artifact list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	kind := fs.String("kind", "", "artifact kind")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: arcmux artifact list [--kind kind]")
	}
	path := "/mesh/artifacts"
	if *kind != "" {
		path += "?kind=" + url.QueryEscape(*kind)
	}
	return writeMeshJSON(cfg, http.MethodGet, path, nil, stdout)
}

func cmdSurface(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: arcmux surface bind|show|list|unbind")
	}
	switch args[0] {
	case "bind":
		return cmdSurfaceBind(args[1:], stdout)
	case "show":
		return cmdSurfaceShow(args[1:], stdout)
	case "list":
		return cmdSurfaceList(args[1:], stdout)
	case "unbind":
		return cmdSurfaceUnbind(args[1:], stdout)
	default:
		return fmt.Errorf("unknown surface subcommand %q", args[0])
	}
}

func cmdSurfaceBind(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) < 3 {
		return errors.New("usage: arcmux surface bind <device> <root|profile:name> <session-id> [--replace]")
	}
	deviceID, profile, sessionID := rest[0], rest[1], rest[2]
	fs := flag.NewFlagSet("surface bind", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	replace := fs.Bool("replace", false, "explicitly replace another target")
	surfaceID := fs.String("surface", os.Getenv("CMUX_SURFACE_ID"), "cmux surface UUID")
	workspaceID := fs.String("workspace", os.Getenv("CMUX_WORKSPACE_ID"), "cmux workspace UUID")
	transportID := fs.String("transport-binding", "", "optional attachment id")
	if err := fs.Parse(rest[3:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *surfaceID == "" || *workspaceID == "" {
		return errors.New("CMUX_SURFACE_ID and CMUX_WORKSPACE_ID are required (or pass --surface and --workspace)")
	}
	parsed, err := cfg.Mesh.Parse()
	if err != nil {
		return err
	}
	registry, err := mesh.LoadRegistry(parsed.RegistryPath)
	if err != nil {
		return err
	}
	if registry.DeviceID == "" {
		return errors.New("local mesh device identity is not configured")
	}
	now := time.Now().UTC()
	bindingID := "bnd-" + strings.ToLower(strings.ReplaceAll(*surfaceID, "-", ""))
	binding := meshstate.SurfaceBinding{
		SchemaVersion: meshstate.SchemaVersion,
		BindingID:     bindingID,
		LocalDeviceID: registry.DeviceID,
		Mux:           "cmux",
		SurfaceID:     *surfaceID,
		WorkspaceID:   *workspaceID,
		Locator: meshstate.RemoteSessionLocator{
			SchemaVersion:      meshstate.SchemaVersion,
			DeviceID:           deviceID,
			ProfileScope:       meshstate.ProfileScope(profile),
			SessionID:          sessionID,
			TransportBindingID: *transportID,
		},
		Source:    "cmux-env",
		CreatedAt: now,
		UpdatedAt: now,
	}
	body, _ := json.Marshal(binding)
	path := "/mesh/surface-bindings"
	if *replace {
		path += "?replace=1"
	}
	return writeMeshJSON(cfg, http.MethodPut, path, body, stdout)
}

func cmdSurfaceShow(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("surface show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	surfaceID := fs.String("surface", os.Getenv("CMUX_SURFACE_ID"), "cmux surface UUID")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 || *surfaceID == "" {
		return errors.New("CMUX_SURFACE_ID is required (or pass --surface)")
	}
	return writeMeshJSON(cfg, http.MethodGet, "/mesh/surface-bindings?resolved=1&surface_id="+url.QueryEscape(*surfaceID), nil, stdout)
}

func cmdSurfaceList(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return errors.New("usage: arcmux surface list")
	}
	return writeMeshJSON(cfg, http.MethodGet, "/mesh/surface-bindings", nil, stdout)
}

func cmdSurfaceUnbind(args []string, stdout io.Writer) error {
	cfg, rest, err := meshConfig(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("surface unbind", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	surfaceID := fs.String("surface", os.Getenv("CMUX_SURFACE_ID"), "cmux surface UUID")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 || *surfaceID == "" {
		return errors.New("CMUX_SURFACE_ID is required (or pass --surface)")
	}
	return writeMeshJSON(cfg, http.MethodDelete, "/mesh/surface-bindings?surface_id="+url.QueryEscape(*surfaceID), nil, stdout)
}

func writeMeshJSON(cfg *config.Config, method, path string, body []byte, stdout io.Writer) error {
	response, err := meshAPIBody(cfg, method, path, body)
	if err != nil {
		return err
	}
	_, err = stdout.Write(response)
	return err
}
