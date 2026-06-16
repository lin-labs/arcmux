package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRecorderAppendsDedupedLines(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "s-test.screen.log")
	var tick int64
	frames := []string{
		"alpha alpha alpha line\nbravo bravo bravo line\n",           // frame 0: 2 new
		"alpha alpha alpha line\nbravo bravo bravo line\n",           // frame 1: idle → 0 new
		"bravo bravo bravo line\ncharlie charlie charlie line\n",     // frame 2: scroll → 1 new
	}
	capture := func(context.Context) (string, error) {
		i := atomic.AddInt64(&tick, 1) - 1
		if int(i) < len(frames) {
			return frames[i], nil
		}
		return frames[len(frames)-1], nil
	}
	r := newRecorder(logPath, capture, 5*time.Millisecond, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	r.start(ctx)
	time.Sleep(60 * time.Millisecond)
	cancel()
	r.stop()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := []string{"alpha alpha alpha line", "bravo bravo bravo line", "charlie charlie charlie line"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("log got %#v want %#v", got, want)
	}
}
