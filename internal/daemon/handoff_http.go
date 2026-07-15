package daemon

import (
	"errors"
	"io"
	"net/http"
)

func (h *HTTPServer) handleMeshHandoffs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		if len(r.URL.Query()) != 0 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "prepare does not accept query parameters"})
			return
		}
		var request sourceHandoffPrepareRequest
		if err := decodeMeshHTTPJSON(w, r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		outbox, err := h.handoffOutbox()
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "handoff outbox is unavailable"})
			return
		}
		prepared, err := outbox.prepare(r.Context(), request)
		if err != nil {
			writeSourceHandoffError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, prepared)
	case http.MethodGet:
		if err := requireOnlyQuery(r, "id"); err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
		outbox, err := h.handoffOutbox()
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "handoff outbox is unavailable"})
			return
		}
		if r.URL.Query().Has("id") {
			id := r.URL.Query().Get("id")
			if id == "" {
				writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id must not be empty"})
				return
			}
			prepared, err := outbox.get(id)
			if err != nil {
				writeSourceHandoffError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, prepared)
			return
		}
		prepared, err := outbox.list()
		if err != nil {
			writeSourceHandoffError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"handoffs": prepared})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "GET or POST required"})
	}
}

func (h *HTTPServer) handleMeshHandoffRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "POST required"})
		return
	}
	if err := requireOnlyQuery(r, "id"); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id is required"})
		return
	}
	if err := requireEmptyBody(r); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	outbox, err := h.handoffOutbox()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "handoff outbox is unavailable"})
		return
	}
	prepared, err := outbox.retry(r.Context(), id)
	if err != nil {
		writeSourceHandoffError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prepared)
}

func (h *HTTPServer) handleMeshHandoffLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "POST required"})
		return
	}
	if err := requireOnlyQuery(r, "id"); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id is required"})
		return
	}
	if err := requireEmptyBody(r); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	outbox, err := h.handoffOutbox()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "handoff outbox is unavailable"})
		return
	}
	launched, err := outbox.launch(r.Context(), id)
	if err != nil {
		writeSourceHandoffError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, launched)
}

func requireOnlyQuery(r *http.Request, allowed string) error {
	for key, values := range r.URL.Query() {
		if key != allowed {
			return errors.New("unknown query parameter")
		}
		if len(values) != 1 {
			return errors.New("query parameter must appear once")
		}
	}
	return nil
}

func requireEmptyBody(r *http.Request) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil {
		return errors.New("read request body")
	}
	if len(data) != 0 {
		return errors.New("request body must be empty")
	}
	return nil
}

func writeSourceHandoffError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch sourceHandoffErrorKindOf(err) {
	case sourceHandoffInvalid:
		status = http.StatusBadRequest
	case sourceHandoffNotFound:
		status = http.StatusNotFound
	case sourceHandoffConflict:
		status = http.StatusConflict
	case sourceHandoffUnavailable:
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, errorResponse{Error: err.Error()})
}
