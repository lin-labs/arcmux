package daemon

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSetRecordingIdempotentAndDecoupled(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()

	sid := "s-rec-1"
	seedSession(d, sid, "agents:1")
	d.captureHook = func(_ context.Context, _ string, _ bool) (string, error) {
		return "stable anchor line one\nstable anchor line two\n", nil
	}

	// Point DataRoot at a temp dir so the log writes there, not ~/data.
	d.cfg.DataRoot = t.TempDir()

	p1, err := d.SetRecording(sid, true)
	if err != nil {
		t.Fatal(err)
	}
	on, p2, _ := d.RecordingStatus(sid)
	if !on || p1 != p2 {
		t.Fatalf("expected recording on with stable path, got on=%v p1=%q p2=%q", on, p1, p2)
	}

	// Idempotent: enabling again returns the same path, no panic/dup loop.
	if p3, err := d.SetRecording(sid, true); err != nil || p3 != p1 {
		t.Fatalf("idempotent enable: p3=%q err=%v", p3, err)
	}

	// Cancel recording.
	if _, err := d.SetRecording(sid, false); err != nil {
		t.Fatal(err)
	}
	if on, _, _ := d.RecordingStatus(sid); on {
		t.Fatal("expected recording off after explicit cancel")
	}

	// The log file is created asynchronously; poll until it appears or timeout.
	var lastStatErr error
	for i := 0; i < 50; i++ {
		_, lastStatErr = os.Stat(p1)
		if lastStatErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastStatErr != nil {
		t.Fatalf("log should be kept after stop: %v", lastStatErr)
	}
}
