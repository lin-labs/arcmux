package daemon

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lin-labs/arcmux/internal/screenstitch"
)

// recorder runs a dedicated capture loop for one session: every interval it
// captures the pane, stitches against the previous frame, and appends only the
// genuinely-new lines to the screen log. Decoupled from any client — it stops
// only when its context is cancelled (explicit cancel or session close).
type recorder struct {
	logPath  string
	capture  func(context.Context) (string, error)
	interval time.Duration
	logger   *slog.Logger
	merger   *screenstitch.Merger

	mu         sync.Mutex
	f          *os.File
	w          *bufio.Writer
	cancel     context.CancelFunc
	done       chan struct{}
	prevStatus string

	startedAt time.Time
}

func newRecorder(logPath string, capture func(context.Context) (string, error), interval time.Duration, logger *slog.Logger) *recorder {
	return &recorder{
		logPath:  logPath,
		capture:  capture,
		interval: interval,
		logger:   logger,
		merger:   screenstitch.NewMerger(0),
		done:     make(chan struct{}),
	}
}

func (r *recorder) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	r.startedAt = time.Now()
	go r.loop(ctx)
}

func (r *recorder) loop(ctx context.Context) {
	defer close(r.done)
	// Truncate/create the log on start (fresh recording per the spec).
	f, err := os.OpenFile(r.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		r.logger.Error("recorder open log failed", "path", r.logPath, "err", err)
		return
	}
	r.mu.Lock()
	r.f, r.w = f, bufio.NewWriter(f)
	r.mu.Unlock()

	t := time.NewTicker(r.interval)
	defer t.Stop()
	r.tick(ctx) // capture immediately, don't wait one interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *recorder) tick(ctx context.Context) {
	raw, err := r.capture(ctx)
	if err != nil {
		r.logger.Debug("recorder capture failed", "path", r.logPath, "err", err)
		return
	}
	lines := r.merger.Ingest(raw)
	status := r.merger.Status()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == nil {
		return
	}
	wrote := false
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		r.w.WriteString(l)
		r.w.WriteByte('\n')
		wrote = true
	}
	// Also emit the live status line when it transitions to a new value
	// (the Merger tracks it separately when the heuristic fires).
	if status != "" && status != r.prevStatus {
		r.w.WriteString(status)
		r.w.WriteByte('\n')
		r.prevStatus = status
		wrote = true
	}
	if wrote {
		r.w.Flush()
	}
}

func (r *recorder) stop() {
	if r.cancel != nil {
		r.cancel()
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w != nil {
		r.w.Flush()
	}
	if r.f != nil {
		r.f.Close()
		r.f, r.w = nil, nil
	}
}
