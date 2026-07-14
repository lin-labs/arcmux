package daemon

import (
	"time"

	"github.com/lin-labs/arcmux/internal/hooks"
	"github.com/lin-labs/arcmux/internal/sessionview"
)

// SessionCatalog exposes one safe, authoritative view across the root daemon
// and all currently running named-profile daemons. The daemon lifecycle state
// is authoritative; hook projection data is best-effort context only.
type SessionCatalog struct {
	root *Daemon
	now  func() time.Time
}

func NewSessionCatalog(root *Daemon) *SessionCatalog {
	return &SessionCatalog{root: root, now: time.Now}
}

// SessionCatalog returns a catalog rooted at this daemon.
func (d *Daemon) SessionCatalog() *SessionCatalog {
	return NewSessionCatalog(d)
}

// List returns one coherent observation. Profile manager membership is
// snapshotted before any child daemon is entered, avoiding lock inversion with
// profile create/remove and daemon shutdown.
func (c *SessionCatalog) List() sessionview.List {
	observedAt := c.now()
	sources := c.sources()
	summaries := make([]sessionview.Summary, 0)
	for _, source := range sources {
		for _, managed := range source.daemon.ListSessions() {
			snap := managed.Snapshot()
			hookState, _ := hooks.ReadSessionState(source.daemon.cfg.Hooks.SessionStateDir, snap.ID)
			summary, _, err := sessionview.Build(source.scope, snap, hookState, observedAt)
			if err != nil {
				// Daemon-created sessions and normalized profile scopes should
				// always be valid. Skip a malformed legacy record rather than
				// failing the complete catalog.
				continue
			}
			summaries = append(summaries, summary)
		}
	}
	sessionview.Sort(summaries)
	return sessionview.List{ObservedAt: observedAt, Sessions: summaries}
}

// Get returns safe detail for exactly one scoped local session.
func (c *SessionCatalog) Get(locator sessionview.Locator) (sessionview.Detail, bool) {
	if err := locator.Validate(); err != nil {
		return sessionview.Detail{}, false
	}
	source, ok := c.source(locator.ProfileScope)
	if !ok {
		return sessionview.Detail{}, false
	}
	managed, ok := source.GetSession(locator.SessionID)
	if !ok {
		return sessionview.Detail{}, false
	}
	snap := managed.Snapshot()
	hookState, _ := hooks.ReadSessionState(source.cfg.Hooks.SessionStateDir, snap.ID)
	_, detail, err := sessionview.Build(locator.ProfileScope, snap, hookState, c.now())
	if err != nil {
		return sessionview.Detail{}, false
	}
	return detail, true
}

type catalogSource struct {
	scope  sessionview.ProfileScope
	daemon *Daemon
}

func (c *SessionCatalog) sources() []catalogSource {
	if c == nil || c.root == nil {
		return nil
	}
	if c.root.cfg.Daemon.ProfileName != "" {
		scope, err := sessionview.NamedProfileScope(c.root.cfg.Daemon.ProfileName)
		if err != nil {
			return nil
		}
		return []catalogSource{{scope: scope, daemon: c.root}}
	}

	sources := []catalogSource{{scope: sessionview.RootProfileScope, daemon: c.root}}
	if c.root.profileManager == nil {
		return sources
	}
	for name, child := range c.root.profileManager.SnapshotDaemons() {
		scope, err := sessionview.NamedProfileScope(name)
		if err != nil || child == nil {
			continue
		}
		sources = append(sources, catalogSource{scope: scope, daemon: child})
	}
	return sources
}

func (c *SessionCatalog) source(scope sessionview.ProfileScope) (*Daemon, bool) {
	if c == nil || c.root == nil {
		return nil, false
	}
	if scope == sessionview.RootProfileScope {
		if c.root.cfg.Daemon.ProfileName != "" {
			return nil, false
		}
		return c.root, true
	}
	name, ok := scope.ProfileName()
	if !ok {
		return nil, false
	}
	if c.root.cfg.Daemon.ProfileName != "" {
		return c.root, c.root.cfg.Daemon.ProfileName == name
	}
	if c.root.profileManager == nil {
		return nil, false
	}
	child, ok := c.root.profileManager.SnapshotDaemons()[name]
	return child, ok && child != nil
}
