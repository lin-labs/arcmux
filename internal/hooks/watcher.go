package hooks

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Watcher monitors hook output files for new events using file tailing.
type Watcher struct {
	outputDir string
	logger    *slog.Logger

	mu       sync.RWMutex
	sessions map[string]*watchedFile
	events   map[string][]HookEvent // sessionID -> recent events
}

type watchedFile struct {
	sessionID string
	path      string
	cancel    context.CancelFunc
}

// NewWatcher creates a hook file watcher.
func NewWatcher(outputDir string, logger *slog.Logger) *Watcher {
	return &Watcher{
		outputDir: outputDir,
		logger:    logger,
		sessions:  make(map[string]*watchedFile),
		events:    make(map[string][]HookEvent),
	}
}

// Watch starts monitoring a hook output file for a session.
func (w *Watcher) Watch(sessionID, filePath string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Stop existing watcher for this session
	if existing, ok := w.sessions[sessionID]; ok {
		existing.cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	wf := &watchedFile{
		sessionID: sessionID,
		path:      filePath,
		cancel:    cancel,
	}
	w.sessions[sessionID] = wf

	go w.tailFile(ctx, wf)
}

// Unwatch stops monitoring a session's hook file.
func (w *Watcher) Unwatch(sessionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if wf, ok := w.sessions[sessionID]; ok {
		wf.cancel()
		delete(w.sessions, sessionID)
	}
}

// LatestEvents returns the most recent hook events for a session.
func (w *Watcher) LatestEvents(sessionID string) []HookEvent {
	w.mu.RLock()
	defer w.mu.RUnlock()

	events := w.events[sessionID]
	if len(events) == 0 {
		return nil
	}

	// Return last 10
	start := 0
	if len(events) > 10 {
		start = len(events) - 10
	}
	result := make([]HookEvent, len(events)-start)
	copy(result, events[start:])
	return result
}

// Run is the main loop (currently a no-op since tailing is per-file).
func (w *Watcher) Run(ctx context.Context) {
	<-ctx.Done()

	// Stop all file watchers
	w.mu.Lock()
	for _, wf := range w.sessions {
		wf.cancel()
	}
	w.sessions = make(map[string]*watchedFile)
	w.mu.Unlock()
}

func (w *Watcher) tailFile(ctx context.Context, wf *watchedFile) {
	// Wait for file to exist
	var f *os.File
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var err error
		f, err = os.Open(wf.path)
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	defer f.Close()

	// Seek to end
	f.Seek(0, io.SeekEnd)

	reader := bufio.NewReader(f)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			w.logger.Warn("hook file read error", "path", wf.path, "error", err)
			return
		}

		event, err := ParseHookEvent(line)
		if err != nil {
			w.logger.Debug("hook event parse error", "line", string(line), "error", err)
			continue
		}

		w.recordEvent(wf.sessionID, event)
	}
}

func (w *Watcher) recordEvent(sessionID string, event HookEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.events[sessionID] = append(w.events[sessionID], event)

	// Cap at 100 events per session
	if len(w.events[sessionID]) > 100 {
		w.events[sessionID] = w.events[sessionID][len(w.events[sessionID])-50:]
	}
}
