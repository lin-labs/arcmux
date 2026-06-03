package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/project"
)

func TestBabysitNewAndContextRoundTrip(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	seedSessionCWD(d, "b1", "vox-a", "/home/blin/Projects/voxtop/VoxtopServer", "")
	seedSessionCWD(d, "b2", "arc-a", "/home/blin/Projects/arcmux", "")
	reg, _ := project.Load(writeProjectsTOML(t, `[[project]]
slug = "voxtop"
repo_cwd = "/home/blin/Projects/voxtop"
`))
	d.projects = reg

	h := &HTTPServer{daemon: d}

	// Mint
	req := httptest.NewRequest(http.MethodPost, "/babysit/new?project=voxtop&server=localhost:5060", nil)
	rec := httptest.NewRecorder()
	h.handleBabysitNew(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("new status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var mint babysitNewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &mint); err != nil {
		t.Fatal(err)
	}
	if mint.Token == "" || mint.Project != "voxtop" {
		t.Fatalf("bad mint: %+v", mint)
	}
	if len(mint.Panes) != 1 || mint.Panes[0].Name != "vox-a" {
		t.Errorf("panes = %+v, want only vox-a", mint.Panes)
	}
	if mint.ConnectURL != "ws://localhost:5060/v1/realtime/converse?context="+mint.Token {
		t.Errorf("connect_url = %q", mint.ConnectURL)
	}

	// Resolve
	req2 := httptest.NewRequest(http.MethodGet, "/babysit/context?context="+mint.Token, nil)
	rec2 := httptest.NewRecorder()
	h.handleBabysitContext(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("context status = %d; body=%s", rec2.Code, rec2.Body.String())
	}
	var ctx BabysitContext
	if err := json.Unmarshal(rec2.Body.Bytes(), &ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Token != mint.Token || ctx.RepoCWD != "/home/blin/Projects/voxtop" {
		t.Errorf("resolved ctx = %+v", ctx)
	}
}

func TestBabysitNewMissingProject(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	h := &HTTPServer{daemon: d}
	rec := httptest.NewRecorder()
	h.handleBabysitNew(rec, httptest.NewRequest(http.MethodPost, "/babysit/new", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestBabysitContextUnknownToken(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	h := &HTTPServer{daemon: d}
	rec := httptest.NewRecorder()
	h.handleBabysitContext(rec, httptest.NewRequest(http.MethodGet, "/babysit/context?context=nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestBabysitContextExpired(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	h := &HTTPServer{daemon: d}
	// Mint with a 1-second TTL, then force expiry by rewriting the stored blob.
	rec := httptest.NewRecorder()
	h.handleBabysitNew(rec, httptest.NewRequest(http.MethodPost, "/babysit/new?project=voxtop&ttl=1", nil))
	var mint babysitNewResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &mint)

	expired := BabysitContext{Token: mint.Token, ExpiresAt: time.Now().Add(-time.Minute)}
	blob, _ := json.Marshal(expired)
	if err := d.State().PutBabysitContext(mint.Token, blob); err != nil {
		t.Fatal(err)
	}

	rec2 := httptest.NewRecorder()
	h.handleBabysitContext(rec2, httptest.NewRequest(http.MethodGet, "/babysit/context?context="+mint.Token, nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (expired)", rec2.Code)
	}
	// Expired token should have been deleted.
	if blob, _ := d.State().GetBabysitContext(mint.Token); blob != nil {
		t.Errorf("expired context should be deleted")
	}
}

func TestConnectURLScheme(t *testing.T) {
	if got := connectURL("labs.example.com:5060", "tok"); got != "wss://labs.example.com:5060/v1/realtime/converse?context=tok" {
		t.Errorf("remote host should be wss: %q", got)
	}
	if got := connectURL("127.0.0.1:5060", "tok"); got != "ws://127.0.0.1:5060/v1/realtime/converse?context=tok" {
		t.Errorf("loopback should be ws: %q", got)
	}
	if got := connectURL("", "tok"); got != "" {
		t.Errorf("empty server should be empty url: %q", got)
	}
}
