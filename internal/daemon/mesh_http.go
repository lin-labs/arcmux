package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/lin-labs/arcmux/internal/meshstate"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

const maxMeshHTTPBody = 64 << 10

func (h *HTTPServer) handleMeshSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "GET required"})
		return
	}
	store, err := h.daemon.meshStateStore()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
		return
	}
	peer := r.URL.Query().Get("peer")
	scope := meshstate.ProfileScope(r.URL.Query().Get("profile"))
	projections, err := store.ListRemoteSessions(peer, scope)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": projections})
}

func (h *HTTPServer) handleMeshSessionsSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "POST required"})
		return
	}
	peer := r.URL.Query().Get("peer")
	if peer == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing peer"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	projections, err := h.daemon.SyncMeshSessions(ctx, peer)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"peer_id": peer, "sessions": projections})
}

func (h *HTTPServer) handleMeshSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "GET required"})
		return
	}
	peer := r.URL.Query().Get("peer")
	scope := meshstate.ProfileScope(r.URL.Query().Get("profile"))
	sessionID := r.URL.Query().Get("session")
	if peer == "" || scope == "" || sessionID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "peer, profile, and session are required"})
		return
	}
	if r.URL.Query().Get("live") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		detail, err := h.daemon.RemoteSessionGet(ctx, peer, sessionview.ProfileScope(scope), sessionID)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, detail)
		return
	}
	store, err := h.daemon.meshStateStore()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
		return
	}
	projection, err := store.GetRemoteSession(meshstate.RemoteSessionLocator{
		SchemaVersion: meshstate.SchemaVersion, DeviceID: peer,
		ProfileScope: scope, SessionID: sessionID,
	})
	if err != nil {
		writeMeshStateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projection)
}

func (h *HTTPServer) handleMeshArtifacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "GET required"})
		return
	}
	store, err := h.daemon.meshStateStore()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
		return
	}
	kind := meshstate.ArtifactKind(r.URL.Query().Get("kind"))
	peer := r.URL.Query().Get("peer")
	var artifacts []meshstate.ArtifactEnvelope
	if peer == "" {
		artifacts, err = store.ListAllArtifacts(kind)
	} else {
		artifacts, err = store.ListArtifactsForOrigin(peer, kind)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": artifacts})
}

func (h *HTTPServer) handleMeshArtifactsSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "POST required"})
		return
	}
	peer := r.URL.Query().Get("peer")
	if peer == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing peer"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	artifacts, err := h.daemon.SyncMeshArtifacts(ctx, peer)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"peer_id": peer, "artifacts": artifacts})
}

func (h *HTTPServer) handleMeshArtifact(w http.ResponseWriter, r *http.Request) {
	store, err := h.daemon.meshStateStore()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
		return
	}
	switch r.Method {
	case http.MethodGet:
		kind := meshstate.ArtifactKind(r.URL.Query().Get("kind"))
		id := r.URL.Query().Get("id")
		if kind == "" || id == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "kind and id are required"})
			return
		}
		if r.URL.Query().Get("live") == "1" {
			peer := r.URL.Query().Get("peer")
			if peer == "" {
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: "peer is required for live artifact get"})
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			artifact, err := h.daemon.RemoteArtifactGet(ctx, peer, kind, id)
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, artifact)
			return
		}
		peer := r.URL.Query().Get("peer")
		var artifact meshstate.ArtifactEnvelope
		if peer == "" {
			artifact, err = store.GetArtifact(kind, id)
		} else {
			artifact, err = store.GetRemoteArtifact(peer, kind, id)
		}
		if err != nil {
			writeMeshStateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, artifact)
	case http.MethodPut:
		var artifact meshstate.ArtifactEnvelope
		if err := decodeMeshHTTPJSON(w, r, &artifact); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		if err := store.PutArtifact(artifact); err != nil {
			writeMeshStateError(w, err)
			return
		}
		stored, _ := store.GetArtifact(artifact.Kind, artifact.ID)
		writeJSON(w, http.StatusOK, stored)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "GET or PUT required"})
	}
}

func (h *HTTPServer) handleMeshSubscribe(w http.ResponseWriter, r *http.Request) {
	peer := r.URL.Query().Get("peer")
	switch r.Method {
	case http.MethodPut:
		var request struct {
			PeerID string   `json:"peer_id"`
			Topics []string `json:"topics"`
		}
		if err := decodeMeshHTTPJSON(w, r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		if request.PeerID == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "peer_id is required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		topics, err := h.daemon.SubscribeMeshPeer(ctx, request.PeerID, request.Topics)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"peer_id": request.PeerID, "topics": topics})
	case http.MethodDelete:
		if peer == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing peer"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := h.daemon.UnsubscribeMeshPeer(ctx, peer); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"peer_id": peer, "topics": []string{}})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "PUT or DELETE required"})
	}
}

func (h *HTTPServer) handleMeshSurfaceBindings(w http.ResponseWriter, r *http.Request) {
	store, err := h.daemon.meshStateStore()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
		return
	}
	surfaceID := r.URL.Query().Get("surface_id")
	switch r.Method {
	case http.MethodGet:
		if surfaceID != "" {
			if r.URL.Query().Get("resolved") == "1" {
				resolved, err := store.ResolveSurfaceBinding(surfaceID)
				if err != nil {
					writeMeshStateError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, resolved)
				return
			}
			binding, err := store.GetSurfaceBinding(surfaceID)
			if err != nil {
				writeMeshStateError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, binding)
			return
		}
		bindings, err := store.ListSurfaceBindings()
		if err != nil {
			writeMeshStateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"surface_bindings": bindings})
	case http.MethodPut:
		var binding meshstate.SurfaceBinding
		if err := decodeMeshHTTPJSON(w, r, &binding); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		localDeviceID := h.daemon.meshDeviceID()
		if localDeviceID == "" {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "local mesh device identity is unavailable"})
			return
		}
		if binding.LocalDeviceID != "" && binding.LocalDeviceID != localDeviceID {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "local_device_id does not match this daemon"})
			return
		}
		binding.LocalDeviceID = localDeviceID
		replace := false
		if raw := r.URL.Query().Get("replace"); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: "replace must be a boolean"})
				return
			}
			replace = parsed
		}
		if err := store.PutSurfaceBinding(binding, replace); err != nil {
			writeMeshStateError(w, err)
			return
		}
		stored, _ := store.GetSurfaceBinding(binding.SurfaceID)
		writeJSON(w, http.StatusOK, stored)
	case http.MethodDelete:
		if surfaceID == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "surface_id is required"})
			return
		}
		if err := store.DeleteSurfaceBinding(surfaceID); err != nil {
			writeMeshStateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "surface_id": surfaceID})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "GET, PUT, or DELETE required"})
	}
}

func (h *HTTPServer) handleMeshValidatedSurfaceBinding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "POST required"})
		return
	}
	store, err := h.daemon.meshStateStore()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: err.Error()})
		return
	}
	var binding meshstate.SurfaceBinding
	if err := decodeMeshHTTPJSON(w, r, &binding); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	localDeviceID := h.daemon.meshDeviceID()
	if localDeviceID == "" {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "local mesh device identity is unavailable"})
		return
	}
	if binding.LocalDeviceID != "" && binding.LocalDeviceID != localDeviceID {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "local_device_id does not match this daemon"})
		return
	}
	binding.LocalDeviceID = localDeviceID
	replace := false
	if raw := r.URL.Query().Get("replace"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "replace must be a boolean"})
			return
		}
		replace = parsed
	}
	resolved, err := store.ValidateAndPutSurfaceBinding(binding, replace)
	if err != nil {
		writeMeshStateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resolved)
}

func decodeMeshHTTPJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxMeshHTTPBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body must contain one JSON value")
	}
	return nil
}

func writeMeshStateError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, meshstate.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, meshstate.ErrConflict):
		status = http.StatusConflict
	}
	writeJSON(w, status, errorResponse{Error: err.Error()})
}
