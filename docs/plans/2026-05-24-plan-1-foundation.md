# Plan 1 — Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `./bin/arcmux manager <agent> <project>` boot a cmux workspace + Elon pane, scaffold the project's durable + ephemeral directories, and open a bbolt store. No tier logic yet — just the substrate.

**Architecture:** New `internal/manager/` package with sub-packages `store` (bbolt DAO), `cmuxcli` (cmux CLI wrapper), `paths` (path resolution), `scaffold` (project bootstrap), `roles` (embedded role-file seeds). New CLI subcommand `manager` in `cmd/arcmux/main.go`. Existing daemon packages untouched.

**Tech Stack:** Go 1.26, `go.etcd.io/bbolt` (pure Go), `cmux` CLI (already installed), `//go:embed` for role file seeds.

**Spec reference:** `~obsAgents/Projects/arcmux/specs/2026-05-24-claude-manager-design.md` §6, §8, §12.

**Follow-on plans (after this ships):**
- Plan 2 — Elon MVP (modes, journal, decisions, prose+JSON parsing)
- Plan 3 — Manager + IC dispatch (contracts, validator, slot model)
- Plan 4 — Comm graph enforcement + crash recovery
- Plan 5 — Retrospectives + principle propagation

---

## File Structure

**Create:**
- `internal/manager/paths/paths.go` — resolves ephemeral + vault paths
- `internal/manager/paths/paths_test.go`
- `internal/manager/store/db.go` — bbolt open/close, bucket creation, schema version
- `internal/manager/store/db_test.go`
- `internal/manager/store/audit.go` — audit log append + range
- `internal/manager/store/audit_test.go`
- `internal/manager/store/teams.go` — team CRUD
- `internal/manager/store/teams_test.go`
- `internal/manager/store/contracts.go` — contract CRUD + state transitions
- `internal/manager/store/contracts_test.go`
- `internal/manager/store/queue.go` — DAG queries (dep checks, cycle detection)
- `internal/manager/store/queue_test.go`
- `internal/manager/store/inbox.go` — inbox push/pop
- `internal/manager/store/inbox_test.go`
- `internal/manager/store/types.go` — Team, Contract, AuditEntry, InboxMsg struct defs (single file to keep DAO files focused)
- `internal/manager/cmuxcli/client.go` — wrap cmux CLI for workspace/pane lifecycle
- `internal/manager/cmuxcli/client_test.go` — uses fake `exec.LookPath` and recorded fixtures
- `internal/manager/cmuxcli/notify.go` — notify, set-status, log, trigger-flash
- `internal/manager/cmuxcli/events.go` — events stream subscribe (subprocess wrapper)
- `internal/manager/roles/seeds.go` — embedded role-file seeds via `//go:embed`
- `internal/manager/roles/files/elon.md` — seeded Elon role
- `internal/manager/roles/files/manager.md` — seeded Manager role
- `internal/manager/roles/files/ic-base.md` — seeded base IC role
- `internal/manager/scaffold/project.go` — creates durable + ephemeral dirs, seeds README/mission/playbook
- `internal/manager/scaffold/project_test.go`
- `internal/manager/scaffold/templates/README.md.tmpl` — seeded project README
- `internal/manager/scaffold/templates/mission.md.tmpl`
- `internal/manager/scaffold/templates/playbook.md.tmpl`
- `internal/manager/project.go` — top-level Project struct that wires store + cmuxcli + paths
- `internal/manager/project_test.go`
- `internal/manager/cmd.go` — the `manager` subcommand handler

**Modify:**
- `cmd/arcmux/main.go` — register `manager` subcommand + printUsage
- `go.mod` — add `go.etcd.io/bbolt` dep
- `Makefile` — no change required

---

### Task 1: Add bbolt dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add bbolt**

```bash
go get go.etcd.io/bbolt@latest
go mod tidy
```

- [ ] **Step 2: Verify the dep landed**

```bash
grep bbolt go.mod
```

Expected output (something like):
```
	go.etcd.io/bbolt v1.x.y
```

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "deps(manager): add bbolt for manager-mode coordination store"
```

---

### Task 2: paths package

**Files:**
- Create: `internal/manager/paths/paths.go`
- Create: `internal/manager/paths/paths_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/manager/paths/paths_test.go`:

```go
package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestForProject(t *testing.T) {
	p := ForProject("/tmp/data", "/tmp/vault", "myproj")

	wantEphemeral := "/tmp/data/arcmux/myproj"
	if p.EphemeralRoot != wantEphemeral {
		t.Errorf("EphemeralRoot = %q, want %q", p.EphemeralRoot, wantEphemeral)
	}

	wantVault := "/tmp/vault/Projects/myproj"
	if p.VaultRoot != wantVault {
		t.Errorf("VaultRoot = %q, want %q", p.VaultRoot, wantVault)
	}

	if !strings.HasSuffix(p.StateBolt, "state.bolt") {
		t.Errorf("StateBolt path %q missing state.bolt suffix", p.StateBolt)
	}

	if filepath.Dir(p.StateBolt) != wantEphemeral {
		t.Errorf("StateBolt dir = %q, want %q", filepath.Dir(p.StateBolt), wantEphemeral)
	}
}

func TestForProject_RejectsBadProject(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if _, err := Validate(""); err == nil {
			t.Error("Validate(\"\") should error")
		}
	})
	t.Run("with slash", func(t *testing.T) {
		if _, err := Validate("foo/bar"); err == nil {
			t.Error("Validate with slash should error")
		}
	})
	t.Run("with dotdot", func(t *testing.T) {
		if _, err := Validate(".."); err == nil {
			t.Error("Validate(\"..\") should error")
		}
	})
	t.Run("valid", func(t *testing.T) {
		got, err := Validate("my-project-1")
		if err != nil {
			t.Errorf("Validate(\"my-project-1\") errored: %v", err)
		}
		if got != "my-project-1" {
			t.Errorf("Validate returned %q, want %q", got, "my-project-1")
		}
	})
}

func TestGlobalRolesDir(t *testing.T) {
	got := GlobalRolesDir("/tmp/vault")
	want := "/tmp/vault/0Prompts/roles"
	if got != want {
		t.Errorf("GlobalRolesDir = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run test, verify fail**

```bash
go test ./internal/manager/paths/...
```

Expected: FAIL (package not built yet).

- [ ] **Step 3: Implement paths.go**

Create `internal/manager/paths/paths.go`:

```go
// Package paths resolves the canonical filesystem locations arcmux's
// manager mode uses, separating machine-local ephemeral state from
// vault-backed durable artifacts.
package paths

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Project bundles every path a manager-mode project needs.
type Project struct {
	Project       string
	EphemeralRoot string // ~/data/arcmux/<project>/
	StateBolt     string // ~/data/arcmux/<project>/state.bolt
	Scratchpads   string // ~/data/arcmux/<project>/scratchpads/
	ConsultInbox  string // ~/data/arcmux/<project>/consult_inboxes/
	Heartbeats    string // ~/data/arcmux/<project>/heartbeats/

	VaultRoot     string // <vault>/Projects/<project>/
	ArcmuxDir     string // <vault>/Projects/<project>/arcmux/
	PrinciplesDir string // <vault>/Projects/<project>/arcmux/principles/
	DeliverDir    string // <vault>/Projects/<project>/arcmux/deliverables/
	ElonDir       string // <vault>/Projects/<project>/elon/
	TeamsDir      string // <vault>/Projects/<project>/teams/
	RetrosDir     string // <vault>/Projects/<project>/retros/
}

// ForProject computes every path given the ephemeral data root, vault root,
// and a validated project slug.
func ForProject(dataRoot, vaultRoot, project string) Project {
	eph := filepath.Join(dataRoot, "arcmux", project)
	v := filepath.Join(vaultRoot, "Projects", project)
	return Project{
		Project:       project,
		EphemeralRoot: eph,
		StateBolt:     filepath.Join(eph, "state.bolt"),
		Scratchpads:   filepath.Join(eph, "scratchpads"),
		ConsultInbox:  filepath.Join(eph, "consult_inboxes"),
		Heartbeats:    filepath.Join(eph, "heartbeats"),
		VaultRoot:     v,
		ArcmuxDir:     filepath.Join(v, "arcmux"),
		PrinciplesDir: filepath.Join(v, "arcmux", "principles"),
		DeliverDir:    filepath.Join(v, "arcmux", "deliverables"),
		ElonDir:       filepath.Join(v, "elon"),
		TeamsDir:      filepath.Join(v, "teams"),
		RetrosDir:     filepath.Join(v, "retros"),
	}
}

// GlobalRolesDir returns the cross-project role library path.
func GlobalRolesDir(vaultRoot string) string {
	return filepath.Join(vaultRoot, "0Prompts", "roles")
}

var projectSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Validate ensures the project slug is filesystem-safe.
func Validate(project string) (string, error) {
	if !projectSlug.MatchString(project) {
		return "", fmt.Errorf("invalid project slug %q: must match [A-Za-z0-9][A-Za-z0-9_.-]{0,63}", project)
	}
	return project, nil
}
```

- [ ] **Step 4: Run test, verify pass**

```bash
go test ./internal/manager/paths/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/paths/
git commit -m "feat(manager): add paths package for project location resolution"
```

---

### Task 3: store types

**Files:**
- Create: `internal/manager/store/types.go`

- [ ] **Step 1: Create the types file**

Create `internal/manager/store/types.go`:

```go
package store

import "time"

// Team is a manager-led group of ICs working a domain.
type Team struct {
	ID         string    `json:"id"`           // e.g. "team-auth-7a2"
	Vision     string    `json:"vision"`       // current charter / vision
	State      string    `json:"state"`        // active | paused | dissolving | archived
	HC         int       `json:"hc"`           // current IC headcount (excludes manager)
	TargetHC   int       `json:"target_hc"`    // manager's intent
	WorkspaceRef string  `json:"workspace_ref"` // cmux ref like "workspace:2"
	ManagerPane string   `json:"manager_pane"`  // cmux pane ref
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Contract is the Anthropic 4-field unit of IC work, plus DAG + lifecycle.
type Contract struct {
	ID                 string    `json:"id"` // e.g. "c-3f1a"
	Team               string    `json:"team"`
	ICRole             string    `json:"ic_role"` // role file name without .md
	Priority           int       `json:"priority"`
	State              string    `json:"state"` // pending|ready|working|blocked|validating|completed|cancelled|failed
	Objective          string    `json:"objective"`
	OutputFormat       string    `json:"output_format"`
	Tools              []string  `json:"tools"`
	Boundaries         []string  `json:"boundaries"`
	AcceptanceCriteria []string  `json:"acceptance_criteria"`
	DependsOn          []string  `json:"depends_on"`
	ParallelizableWith []string  `json:"parallelizable_with"`
	Capstone           bool      `json:"capstone"`
	Deadline           *time.Time `json:"deadline,omitempty"`
	Validations        []Validation `json:"validations"`
	Audit              []ContractAudit `json:"audit"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// Validation is one validator pass on a contract.
type Validation struct {
	Timestamp time.Time `json:"ts"`
	By        string    `json:"by"`       // validator IC slot id
	Verdict   string    `json:"verdict"`  // pass | needs-work
	Feedback  string    `json:"feedback"`
}

// ContractAudit records a state transition.
type ContractAudit struct {
	Timestamp time.Time `json:"ts"`
	State     string    `json:"state"`
	By        string    `json:"by"`
	Reason    string    `json:"reason,omitempty"`
}

// AuditEntry is a project-wide audit row.
type AuditEntry struct {
	Timestamp time.Time              `json:"ts"`
	Action    string                 `json:"action"`
	Actor     string                 `json:"actor"`
	Subject   string                 `json:"subject"`
	RuleID    string                 `json:"rule_id,omitempty"`
	Detail    map[string]any         `json:"detail,omitempty"`
}

// InboxMsg is a queued message awaiting Elon/Manager processing.
type InboxMsg struct {
	ID       string         `json:"id"`
	Verb     string         `json:"verb"` // add|revise|retract|consult|escalate|...
	From     string         `json:"from"` // user|elon|manager-<slug>|ic-<id>|arcmux
	Priority int            `json:"priority"`
	Body     string         `json:"body"`
	Refs     map[string]any `json:"refs,omitempty"`
	ReceivedAt time.Time    `json:"received_at"`
}

// Valid contract states.
const (
	ContractPending    = "pending"
	ContractReady      = "ready"
	ContractWorking    = "working"
	ContractBlocked    = "blocked"
	ContractValidating = "validating"
	ContractCompleted  = "completed"
	ContractCancelled  = "cancelled"
	ContractFailed     = "failed"
)

// Valid team states.
const (
	TeamActive     = "active"
	TeamPaused     = "paused"
	TeamDissolving = "dissolving"
	TeamArchived   = "archived"
)
```

- [ ] **Step 2: Verify compiles**

```bash
go build ./internal/manager/store/...
```

Expected: success (no test yet, types only).

- [ ] **Step 3: Commit**

```bash
git add internal/manager/store/types.go
git commit -m "feat(manager/store): add Team, Contract, AuditEntry, InboxMsg types"
```

---

### Task 4: store/db.go — open/close/buckets

**Files:**
- Create: `internal/manager/store/db.go`
- Create: `internal/manager/store/db_test.go`

- [ ] **Step 1: Write failing test**

Create `internal/manager/store/db_test.go`:

```go
package store

import (
	"path/filepath"
	"testing"
)

func TestOpenCreatesBuckets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.bolt")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open errored: %v", err)
	}
	defer db.Close()

	for _, b := range AllBuckets {
		if !db.HasBucket(b) {
			t.Errorf("expected bucket %q to exist", b)
		}
	}
}

func TestOpenSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.bolt")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open errored: %v", err)
	}
	defer db.Close()

	v, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion errored: %v", err)
	}
	if v != CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", v, CurrentSchemaVersion)
	}
}

func TestReopenIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.bolt")

	for i := 0; i < 3; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d errored: %v", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close #%d errored: %v", i, err)
		}
	}
}
```

- [ ] **Step 2: Run, verify fail**

```bash
go test ./internal/manager/store/...
```

Expected: FAIL.

- [ ] **Step 3: Implement db.go**

Create `internal/manager/store/db.go`:

```go
// Package store implements the per-project coordination data store backed
// by bbolt. It owns contracts, teams, audit log, inboxes, and DAG indices.
package store

import (
	"encoding/binary"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// CurrentSchemaVersion is the on-disk schema version this binary expects.
const CurrentSchemaVersion = 1

// Bucket names.
const (
	BucketTeams           = "teams"
	BucketContracts       = "contracts"
	BucketIdxTeamContract = "idx-team-contract"
	BucketIdxDepsParent   = "idx-deps-parent"
	BucketIdxDepsChild    = "idx-deps-child"
	BucketIdxState        = "idx-state"
	BucketIdxPriority     = "idx-priority"
	BucketInboxElon       = "inbox-elon"
	BucketInboxManagerPfx = "inbox-manager-" // suffixed with team slug
	BucketAudit           = "audit"
	BucketMeta            = "meta"
)

// AllBuckets lists buckets created on Open.
var AllBuckets = []string{
	BucketTeams,
	BucketContracts,
	BucketIdxTeamContract,
	BucketIdxDepsParent,
	BucketIdxDepsChild,
	BucketIdxState,
	BucketIdxPriority,
	BucketInboxElon,
	BucketAudit,
	BucketMeta,
}

// DB wraps a bbolt handle with arcmux-specific helpers.
type DB struct {
	b *bolt.DB
}

// Open opens or creates a bbolt file at path and ensures schema is current.
func Open(path string) (*DB, error) {
	b, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("bbolt open %s: %w", path, err)
	}
	db := &DB{b: b}
	if err := db.init(); err != nil {
		b.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the underlying file handle.
func (d *DB) Close() error { return d.b.Close() }

// Bolt exposes the raw handle for advanced callers (tests, snapshots).
func (d *DB) Bolt() *bolt.DB { return d.b }

func (d *DB) init() error {
	return d.b.Update(func(tx *bolt.Tx) error {
		for _, name := range AllBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}

		meta := tx.Bucket([]byte(BucketMeta))
		v := meta.Get([]byte("schema-version"))
		if v == nil {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], CurrentSchemaVersion)
			return meta.Put([]byte("schema-version"), buf[:])
		}
		got := binary.BigEndian.Uint64(v)
		if got != CurrentSchemaVersion {
			return fmt.Errorf("schema version mismatch: file=%d, binary=%d", got, CurrentSchemaVersion)
		}
		return nil
	})
}

// HasBucket reports whether a bucket exists. Test helper.
func (d *DB) HasBucket(name string) bool {
	var ok bool
	_ = d.b.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket([]byte(name)) != nil
		return nil
	})
	return ok
}

// SchemaVersion returns the persisted schema version.
func (d *DB) SchemaVersion() (uint64, error) {
	var v uint64
	err := d.b.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(BucketMeta))
		raw := meta.Get([]byte("schema-version"))
		if len(raw) != 8 {
			return fmt.Errorf("schema-version missing or malformed")
		}
		v = binary.BigEndian.Uint64(raw)
		return nil
	})
	return v, err
}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/manager/store/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/store/db.go internal/manager/store/db_test.go
git commit -m "feat(manager/store): open bbolt with versioned schema and buckets"
```

---

### Task 5: store/audit.go

**Files:**
- Create: `internal/manager/store/audit.go`
- Create: `internal/manager/store/audit_test.go`

- [ ] **Step 1: Failing test**

Create `internal/manager/store/audit_test.go`:

```go
package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAuditAppendAndRange(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	e1 := AuditEntry{Timestamp: time.Now(), Action: "team-created", Actor: "elon", Subject: "team-a"}
	e2 := AuditEntry{Timestamp: time.Now().Add(time.Millisecond), Action: "ic-spawned", Actor: "manager-a", Subject: "ic-1"}

	if err := db.AppendAudit(e1); err != nil {
		t.Fatalf("AppendAudit e1: %v", err)
	}
	if err := db.AppendAudit(e2); err != nil {
		t.Fatalf("AppendAudit e2: %v", err)
	}

	all, err := db.RecentAudit(10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d entries, want 2", len(all))
	}
	// Recent should be newest-first.
	if all[0].Action != "ic-spawned" {
		t.Errorf("recent[0].Action = %q, want %q", all[0].Action, "ic-spawned")
	}
	if all[1].Action != "team-created" {
		t.Errorf("recent[1].Action = %q, want %q", all[1].Action, "team-created")
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.bolt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}
```

- [ ] **Step 2: Run, verify fail**

```bash
go test ./internal/manager/store/... -run Audit
```

Expected: FAIL.

- [ ] **Step 3: Implement audit.go**

Create `internal/manager/store/audit.go`:

```go
package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// AppendAudit writes an audit entry. Key is a sortable ts-uuid composite;
// here we use the nano timestamp followed by an autoincrement suffix from
// the bucket's NextSequence so identical-nano writes don't collide.
func (d *DB) AppendAudit(e AuditEntry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAudit))
		seq, err := b.NextSequence()
		if err != nil {
			return fmt.Errorf("audit nextseq: %w", err)
		}
		key := auditKey(e.Timestamp, seq)
		val, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return b.Put(key, val)
	})
}

// RecentAudit returns up to n entries, newest-first.
func (d *DB) RecentAudit(n int) ([]AuditEntry, error) {
	if n <= 0 {
		return nil, nil
	}
	out := make([]AuditEntry, 0, n)
	err := d.b.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(BucketAudit)).Cursor()
		for k, v := c.Last(); k != nil && len(out) < n; k, v = c.Prev() {
			var e AuditEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

func auditKey(ts time.Time, seq uint64) []byte {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(ts.UnixNano()))
	binary.BigEndian.PutUint64(buf[8:], seq)
	return buf[:]
}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/manager/store/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/store/audit.go internal/manager/store/audit_test.go
git commit -m "feat(manager/store): append-only audit log with newest-first range"
```

---

### Task 6: store/teams.go

**Files:**
- Create: `internal/manager/store/teams.go`
- Create: `internal/manager/store/teams_test.go`

- [ ] **Step 1: Failing test**

Create `internal/manager/store/teams_test.go`:

```go
package store

import (
	"testing"
	"time"
)

func TestTeamPutGet(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	team := Team{
		ID:        "team-foo-7a2",
		Vision:    "Handle auth refactor",
		State:     TeamActive,
		HC:        2,
		TargetHC:  3,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := db.PutTeam(team); err != nil {
		t.Fatalf("PutTeam: %v", err)
	}

	got, err := db.GetTeam("team-foo-7a2")
	if err != nil {
		t.Fatalf("GetTeam: %v", err)
	}
	if got.Vision != team.Vision || got.HC != team.HC {
		t.Errorf("got %+v, want %+v", got, team)
	}
}

func TestTeamNotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, err := db.GetTeam("missing")
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListTeams(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	now := time.Now()
	_ = db.PutTeam(Team{ID: "team-a", State: TeamActive, CreatedAt: now, UpdatedAt: now})
	_ = db.PutTeam(Team{ID: "team-b", State: TeamActive, CreatedAt: now, UpdatedAt: now})
	_ = db.PutTeam(Team{ID: "team-c", State: TeamArchived, CreatedAt: now, UpdatedAt: now})

	all, err := db.ListTeams("")
	if err != nil {
		t.Fatalf("ListTeams all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ListTeams all = %d, want 3", len(all))
	}

	active, err := db.ListTeams(TeamActive)
	if err != nil {
		t.Fatalf("ListTeams active: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("ListTeams active = %d, want 2", len(active))
	}
}
```

- [ ] **Step 2: Run, verify fail**

```bash
go test ./internal/manager/store/... -run Team
```

Expected: FAIL.

- [ ] **Step 3: Implement teams.go**

Create `internal/manager/store/teams.go`:

```go
package store

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrNotFound is returned when a lookup misses.
var ErrNotFound = errors.New("not found")

// PutTeam upserts a team document.
func (d *DB) PutTeam(t Team) error {
	if t.ID == "" {
		return errors.New("team.ID required")
	}
	t.UpdatedAt = time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = t.UpdatedAt
	}
	val, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketTeams)).Put([]byte(t.ID), val)
	})
}

// GetTeam returns the team by ID or ErrNotFound.
func (d *DB) GetTeam(id string) (Team, error) {
	var t Team
	err := d.b.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketTeams)).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &t)
	})
	return t, err
}

// ListTeams returns teams optionally filtered by state ("" = all).
func (d *DB) ListTeams(state string) ([]Team, error) {
	var out []Team
	err := d.b.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketTeams)).ForEach(func(_, v []byte) error {
			var t Team
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			if state == "" || t.State == state {
				out = append(out, t)
			}
			return nil
		})
	})
	return out, err
}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/manager/store/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/store/teams.go internal/manager/store/teams_test.go
git commit -m "feat(manager/store): team CRUD with ListTeams filter by state"
```

---

### Task 7: store/contracts.go

**Files:**
- Create: `internal/manager/store/contracts.go`
- Create: `internal/manager/store/contracts_test.go`

- [ ] **Step 1: Failing test**

Create `internal/manager/store/contracts_test.go`:

```go
package store

import (
	"testing"
	"time"
)

func TestContractPutGet(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	c := Contract{
		ID:                 "c-1",
		Team:               "team-a",
		ICRole:             "linus",
		Priority:           2,
		State:              ContractPending,
		Objective:          "Do X",
		OutputFormat:       "PR",
		Tools:              []string{"bash", "edit"},
		AcceptanceCriteria: []string{"tests pass"},
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	if err := db.PutContract(c); err != nil {
		t.Fatalf("PutContract: %v", err)
	}

	got, err := db.GetContract("c-1")
	if err != nil {
		t.Fatalf("GetContract: %v", err)
	}
	if got.Objective != c.Objective {
		t.Errorf("got %q, want %q", got.Objective, c.Objective)
	}
}

func TestContractIndexesPopulated(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	c := Contract{
		ID:        "c-1",
		Team:      "team-a",
		Priority:  3,
		State:     ContractPending,
		DependsOn: []string{"c-0"},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := db.PutContract(c); err != nil {
		t.Fatalf("PutContract: %v", err)
	}

	// idx-team-contract: team-a/c-1
	teamCs, err := db.ListContractsByTeam("team-a")
	if err != nil {
		t.Fatalf("ListContractsByTeam: %v", err)
	}
	if len(teamCs) != 1 || teamCs[0] != "c-1" {
		t.Errorf("ListContractsByTeam = %v, want [c-1]", teamCs)
	}

	// idx-state: pending/c-1
	pendingCs, err := db.ListContractsByState(ContractPending)
	if err != nil {
		t.Fatalf("ListContractsByState: %v", err)
	}
	if len(pendingCs) != 1 {
		t.Errorf("pending contracts = %v, want [c-1]", pendingCs)
	}

	// idx-deps-parent: c-0/c-1 (forward: c-0 has child c-1)
	children, err := db.ChildrenOf("c-0")
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(children) != 1 || children[0] != "c-1" {
		t.Errorf("ChildrenOf(c-0) = %v, want [c-1]", children)
	}
}

func TestContractTransitionValid(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	c := Contract{ID: "c-1", Team: "team-a", State: ContractPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	_ = db.PutContract(c)

	if err := db.TransitionContract("c-1", ContractReady, "deps-met", "arcmux"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	got, _ := db.GetContract("c-1")
	if got.State != ContractReady {
		t.Errorf("state = %q, want %q", got.State, ContractReady)
	}
	if len(got.Audit) != 1 {
		t.Errorf("audit length = %d, want 1", len(got.Audit))
	}

	// idx-state should now have ready/c-1 not pending/c-1
	pendingCs, _ := db.ListContractsByState(ContractPending)
	if len(pendingCs) != 0 {
		t.Errorf("pending after transition = %v, want []", pendingCs)
	}
	readyCs, _ := db.ListContractsByState(ContractReady)
	if len(readyCs) != 1 {
		t.Errorf("ready after transition = %v, want [c-1]", readyCs)
	}
}

func TestContractTransitionDepsNotMet(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	c0 := Contract{ID: "c-0", Team: "team-a", State: ContractPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	c1 := Contract{ID: "c-1", Team: "team-a", State: ContractPending, DependsOn: []string{"c-0"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	_ = db.PutContract(c0)
	_ = db.PutContract(c1)

	if err := db.TransitionContract("c-1", ContractWorking, "go", "manager"); err == nil {
		t.Error("expected error transitioning to working with unmet dep")
	}
}
```

- [ ] **Step 2: Run, verify fail**

```bash
go test ./internal/manager/store/... -run Contract
```

Expected: FAIL.

- [ ] **Step 3: Implement contracts.go**

Create `internal/manager/store/contracts.go`:

```go
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// PutContract upserts a contract document and (re)builds its indexes.
func (d *DB) PutContract(c Contract) error {
	if c.ID == "" {
		return errors.New("contract.ID required")
	}
	if c.State == "" {
		c.State = ContractPending
	}
	c.UpdatedAt = time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = c.UpdatedAt
	}

	return d.b.Update(func(tx *bolt.Tx) error {
		// Remove any prior index rows for this contract.
		if prior := tx.Bucket([]byte(BucketContracts)).Get([]byte(c.ID)); prior != nil {
			var old Contract
			if err := json.Unmarshal(prior, &old); err == nil {
				clearContractIndexes(tx, old)
			}
		}
		// Write the main doc.
		val, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte(BucketContracts)).Put([]byte(c.ID), val); err != nil {
			return err
		}
		return writeContractIndexes(tx, c)
	})
}

// GetContract returns the contract by ID or ErrNotFound.
func (d *DB) GetContract(id string) (Contract, error) {
	var c Contract
	err := d.b.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(BucketContracts)).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &c)
	})
	return c, err
}

// ListContractsByTeam returns contract IDs for a team via the index.
func (d *DB) ListContractsByTeam(team string) ([]string, error) {
	return d.listByPrefix(BucketIdxTeamContract, team+"/", 1)
}

// ListContractsByState returns contract IDs in a given state via the index.
func (d *DB) ListContractsByState(state string) ([]string, error) {
	return d.listByPrefix(BucketIdxState, state+"/", 1)
}

// ChildrenOf returns contracts that depend on parent.
func (d *DB) ChildrenOf(parent string) ([]string, error) {
	return d.listByPrefix(BucketIdxDepsParent, parent+"/", 1)
}

// ParentsOf returns contracts that child depends on.
func (d *DB) ParentsOf(child string) ([]string, error) {
	return d.listByPrefix(BucketIdxDepsChild, child+"/", 1)
}

// TransitionContract changes a contract's state with validation and audit.
// Dependency gates are enforced for pending→working and pending→ready transitions
// that imply deps must be completed.
func (d *DB) TransitionContract(id, newState, reason, by string) error {
	return d.b.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(BucketContracts))
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var c Contract
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}

		if !validTransition(c.State, newState) {
			return fmt.Errorf("invalid transition %q → %q for contract %s", c.State, newState, id)
		}

		// Gate transitions into working state on deps-completed.
		if newState == ContractWorking || newState == ContractReady {
			for _, parent := range c.DependsOn {
				p := bucket.Get([]byte(parent))
				if p == nil {
					return fmt.Errorf("dep %q missing for contract %s", parent, id)
				}
				var pc Contract
				if err := json.Unmarshal(p, &pc); err != nil {
					return err
				}
				if pc.State != ContractCompleted {
					return fmt.Errorf("dep %q not completed (state=%s) for contract %s", parent, pc.State, id)
				}
			}
		}

		clearContractIndexes(tx, c)
		oldState := c.State
		c.State = newState
		c.UpdatedAt = time.Now()
		c.Audit = append(c.Audit, ContractAudit{
			Timestamp: c.UpdatedAt,
			State:     newState,
			By:        by,
			Reason:    reason,
		})
		_ = oldState

		val, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(id), val); err != nil {
			return err
		}
		return writeContractIndexes(tx, c)
	})
}

// validTransition implements the contract state machine guard.
func validTransition(from, to string) bool {
	if from == to {
		return false
	}
	allowed := map[string]map[string]bool{
		ContractPending:    {ContractReady: true, ContractCancelled: true},
		ContractReady:      {ContractWorking: true, ContractCancelled: true},
		ContractWorking:    {ContractBlocked: true, ContractValidating: true, ContractCancelled: true, ContractFailed: true},
		ContractBlocked:    {ContractWorking: true, ContractCancelled: true, ContractFailed: true},
		ContractValidating: {ContractCompleted: true, ContractWorking: true, ContractFailed: true},
		ContractCompleted:  {},
		ContractCancelled:  {},
		ContractFailed:     {},
	}
	next, ok := allowed[from]
	if !ok {
		return false
	}
	return next[to]
}

func writeContractIndexes(tx *bolt.Tx, c Contract) error {
	// idx-team-contract: <team>/<id>
	if c.Team != "" {
		key := []byte(c.Team + "/" + c.ID)
		if err := tx.Bucket([]byte(BucketIdxTeamContract)).Put(key, nil); err != nil {
			return err
		}
	}
	// idx-state: <state>/<id>
	if c.State != "" {
		key := []byte(c.State + "/" + c.ID)
		if err := tx.Bucket([]byte(BucketIdxState)).Put(key, nil); err != nil {
			return err
		}
	}
	// idx-priority: <priority>/<id>
	pkey := []byte(fmt.Sprintf("%02d/%s", c.Priority, c.ID))
	if err := tx.Bucket([]byte(BucketIdxPriority)).Put(pkey, nil); err != nil {
		return err
	}
	// idx-deps-parent: <parent>/<child>
	for _, p := range c.DependsOn {
		key := []byte(p + "/" + c.ID)
		if err := tx.Bucket([]byte(BucketIdxDepsParent)).Put(key, nil); err != nil {
			return err
		}
		// idx-deps-child: <child>/<parent>
		ck := []byte(c.ID + "/" + p)
		if err := tx.Bucket([]byte(BucketIdxDepsChild)).Put(ck, nil); err != nil {
			return err
		}
	}
	return nil
}

func clearContractIndexes(tx *bolt.Tx, c Contract) {
	if c.Team != "" {
		_ = tx.Bucket([]byte(BucketIdxTeamContract)).Delete([]byte(c.Team + "/" + c.ID))
	}
	if c.State != "" {
		_ = tx.Bucket([]byte(BucketIdxState)).Delete([]byte(c.State + "/" + c.ID))
	}
	_ = tx.Bucket([]byte(BucketIdxPriority)).Delete([]byte(fmt.Sprintf("%02d/%s", c.Priority, c.ID)))
	for _, p := range c.DependsOn {
		_ = tx.Bucket([]byte(BucketIdxDepsParent)).Delete([]byte(p + "/" + c.ID))
		_ = tx.Bucket([]byte(BucketIdxDepsChild)).Delete([]byte(c.ID + "/" + p))
	}
}

// listByPrefix returns the suffix of keys in bucket starting with prefix.
// part indicates which "/"-segment to return (0-indexed).
func (d *DB) listByPrefix(bucket, prefix string, part int) ([]string, error) {
	var out []string
	err := d.b.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucket)).Cursor()
		bp := []byte(prefix)
		for k, _ := c.Seek(bp); k != nil && hasPrefix(k, bp); k, _ = c.Next() {
			parts := strings.SplitN(string(k), "/", part+2)
			if len(parts) > part {
				out = append(out, parts[part+1])
			}
		}
		return nil
	})
	return out, err
}

func hasPrefix(b, p []byte) bool {
	if len(b) < len(p) {
		return false
	}
	for i := range p {
		if b[i] != p[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/manager/store/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/store/contracts.go internal/manager/store/contracts_test.go
git commit -m "feat(manager/store): contract CRUD + indexes + state-machine transitions with dep gates"
```

---

### Task 8: store/inbox.go

**Files:**
- Create: `internal/manager/store/inbox.go`
- Create: `internal/manager/store/inbox_test.go`

- [ ] **Step 1: Failing test**

```go
package store

import (
	"testing"
	"time"
)

func TestInboxPushPop(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	m1 := InboxMsg{ID: "m1", Verb: "add", From: "user", Priority: 1, Body: "do X", ReceivedAt: time.Now()}
	m2 := InboxMsg{ID: "m2", Verb: "revise", From: "user", Priority: 2, Body: "actually Y", ReceivedAt: time.Now().Add(time.Millisecond)}

	if err := db.PushElonInbox(m1); err != nil {
		t.Fatalf("PushElonInbox m1: %v", err)
	}
	if err := db.PushElonInbox(m2); err != nil {
		t.Fatalf("PushElonInbox m2: %v", err)
	}

	msgs, err := db.PeekElonInbox(10)
	if err != nil {
		t.Fatalf("PeekElonInbox: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2", len(msgs))
	}
	// Oldest first (FIFO for processing order).
	if msgs[0].ID != "m1" {
		t.Errorf("msgs[0].ID = %q, want m1", msgs[0].ID)
	}

	if err := db.AckElonInbox(msgs[0].ID); err != nil {
		t.Fatalf("AckElonInbox: %v", err)
	}
	remaining, _ := db.PeekElonInbox(10)
	if len(remaining) != 1 || remaining[0].ID != "m2" {
		t.Errorf("after ack remaining = %+v, want [m2]", remaining)
	}
}
```

- [ ] **Step 2: Run, verify fail**

```bash
go test ./internal/manager/store/... -run Inbox
```

Expected: FAIL.

- [ ] **Step 3: Implement inbox.go**

```go
package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// PushElonInbox enqueues a message to Elon's inbox.
func (d *DB) PushElonInbox(m InboxMsg) error {
	return d.push(BucketInboxElon, m)
}

// PeekElonInbox returns up to n messages oldest-first without removing them.
func (d *DB) PeekElonInbox(n int) ([]InboxMsg, error) {
	return d.peek(BucketInboxElon, n)
}

// AckElonInbox removes a single message by ID.
func (d *DB) AckElonInbox(id string) error {
	return d.ack(BucketInboxElon, id)
}

func (d *DB) push(bucket string, m InboxMsg) error {
	if m.ID == "" {
		return fmt.Errorf("InboxMsg.ID required")
	}
	if m.ReceivedAt.IsZero() {
		m.ReceivedAt = time.Now()
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %s missing", bucket)
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		var keybuf [16]byte
		binary.BigEndian.PutUint64(keybuf[:8], uint64(m.ReceivedAt.UnixNano()))
		binary.BigEndian.PutUint64(keybuf[8:], seq)
		val, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return b.Put(keybuf[:], val)
	})
}

func (d *DB) peek(bucket string, n int) ([]InboxMsg, error) {
	if n <= 0 {
		return nil, nil
	}
	out := make([]InboxMsg, 0, n)
	err := d.b.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucket)).Cursor()
		for k, v := c.First(); k != nil && len(out) < n; k, v = c.Next() {
			var m InboxMsg
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return nil
	})
	return out, err
}

func (d *DB) ack(bucket, id string) error {
	return d.b.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var m InboxMsg
			if err := json.Unmarshal(v, &m); err != nil {
				continue
			}
			if m.ID == id {
				return b.Delete(k)
			}
		}
		return ErrNotFound
	})
}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/manager/store/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/store/inbox.go internal/manager/store/inbox_test.go
git commit -m "feat(manager/store): Elon inbox push/peek/ack with FIFO order"
```

---

### Task 9: cmuxcli — interface + real CLI client

**Files:**
- Create: `internal/manager/cmuxcli/client.go`
- Create: `internal/manager/cmuxcli/client_test.go`

- [ ] **Step 1: Failing test using a fake runner**

Create `internal/manager/cmuxcli/client_test.go`:

```go
package cmuxcli

import (
	"context"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls   [][]string
	out     string
	err     error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	return f.out, f.err
}

func TestNewWorkspaceJSON(t *testing.T) {
	f := &fakeRunner{out: `{"workspace":{"ref":"workspace:2","uuid":"abc"}}`}
	c := newWithRunner(f)
	ws, err := c.NewWorkspace(context.Background(), "team-foo")
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	if ws.Ref != "workspace:2" {
		t.Errorf("ref = %q, want workspace:2", ws.Ref)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.calls))
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "new-workspace") || !strings.Contains(got, "team-foo") {
		t.Errorf("call = %q, expected to contain new-workspace and team-foo", got)
	}
}

func TestSend(t *testing.T) {
	f := &fakeRunner{out: ""}
	c := newWithRunner(f)
	if err := c.Send(context.Background(), "surface:5", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.calls))
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "send") || !strings.Contains(got, "surface:5") {
		t.Errorf("call = %q", got)
	}
}
```

- [ ] **Step 2: Run, verify fail**

```bash
go test ./internal/manager/cmuxcli/...
```

Expected: FAIL.

- [ ] **Step 3: Implement client.go**

```go
// Package cmuxcli wraps the cmux command-line tool. Every method shells out
// to the cmux binary; the runner interface lets tests substitute a fake.
package cmuxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// Runner abstracts process execution for testability.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

type execRunner struct{ bin string }

func (e *execRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.bin, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s %v: %s", e.bin, args, string(ee.Stderr))
		}
		return "", err
	}
	return string(out), nil
}

// Client talks to a local cmux daemon via its CLI.
type Client struct {
	r Runner
}

// New returns a Client that shells out to the `cmux` binary.
func New() *Client {
	return &Client{r: &execRunner{bin: "cmux"}}
}

func newWithRunner(r Runner) *Client { return &Client{r: r} }

// Workspace identifies a cmux workspace.
type Workspace struct {
	Ref  string `json:"ref"`
	UUID string `json:"uuid"`
}

type wsCreateOut struct {
	Workspace Workspace `json:"workspace"`
}

// NewWorkspace creates a cmux workspace with the given name.
func (c *Client) NewWorkspace(ctx context.Context, name string) (Workspace, error) {
	out, err := c.r.Run(ctx, "--json", "new-workspace", "--name", name)
	if err != nil {
		return Workspace{}, fmt.Errorf("cmux new-workspace: %w", err)
	}
	var v wsCreateOut
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		// Some cmux versions print just the ref on stdout.
		return Workspace{Ref: trimSpace(out)}, nil
	}
	return v.Workspace, nil
}

// Pane identifies a cmux pane.
type Pane struct {
	Ref string `json:"ref"`
}

// NewPane creates a pane inside a workspace, optionally running command.
func (c *Client) NewPane(ctx context.Context, workspaceRef, cwd, command string) (Pane, error) {
	args := []string{"--json", "new-pane", "--workspace", workspaceRef}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	if command != "" {
		args = append(args, "--command", command)
	}
	out, err := c.r.Run(ctx, args...)
	if err != nil {
		return Pane{}, fmt.Errorf("cmux new-pane: %w", err)
	}
	var v struct {
		Pane Pane `json:"pane"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return Pane{Ref: trimSpace(out)}, nil
	}
	return v.Pane, nil
}

// Send pushes text into a surface/pane reference.
func (c *Client) Send(ctx context.Context, target, text string) error {
	_, err := c.r.Run(ctx, "send", "--target", target, "--", text)
	if err != nil {
		return fmt.Errorf("cmux send: %w", err)
	}
	return nil
}

// CloseSurface closes a surface.
func (c *Client) CloseSurface(ctx context.Context, target string) error {
	_, err := c.r.Run(ctx, "close-surface", "--surface", target)
	if err != nil {
		return fmt.Errorf("cmux close-surface: %w", err)
	}
	return nil
}

// FocusPane focuses a pane/surface.
func (c *Client) FocusPane(ctx context.Context, target string) error {
	_, err := c.r.Run(ctx, "focus-pane", "--target", target)
	if err != nil {
		return fmt.Errorf("cmux focus-pane: %w", err)
	}
	return nil
}

// ReadScreen returns terminal text from a surface.
func (c *Client) ReadScreen(ctx context.Context, target string) (string, error) {
	out, err := c.r.Run(ctx, "read-screen", "--surface", target)
	if err != nil {
		return "", fmt.Errorf("cmux read-screen: %w", err)
	}
	return out, nil
}

// Identify reports server identity + caller context.
func (c *Client) Identify(ctx context.Context) (string, error) {
	return c.r.Run(ctx, "--json", "identify")
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s
}
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/manager/cmuxcli/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/cmuxcli/
git commit -m "feat(manager/cmuxcli): client wrapping cmux CLI with Runner abstraction"
```

---

### Task 10: cmuxcli/notify.go

**Files:**
- Create: `internal/manager/cmuxcli/notify.go`

- [ ] **Step 1: Implement (test extends client_test.go style — keep brief)**

```go
package cmuxcli

import (
	"context"
	"fmt"
)

// Notify fires a user-attention notification on the given workspace/surface.
func (c *Client) Notify(ctx context.Context, target, title, body string) error {
	_, err := c.r.Run(ctx, "notify", "--target", target, "--title", title, "--body", body)
	if err != nil {
		return fmt.Errorf("cmux notify: %w", err)
	}
	return nil
}

// SetStatus sets a sidebar status pill on a surface.
func (c *Client) SetStatus(ctx context.Context, target, label string) error {
	_, err := c.r.Run(ctx, "set-status", "--target", target, "--label", label)
	if err != nil {
		return fmt.Errorf("cmux set-status: %w", err)
	}
	return nil
}

// SetProgress sets sidebar progress (0.0–1.0).
func (c *Client) SetProgress(ctx context.Context, target string, pct float64) error {
	_, err := c.r.Run(ctx, "set-progress", "--target", target, "--value", fmt.Sprintf("%.2f", pct))
	if err != nil {
		return fmt.Errorf("cmux set-progress: %w", err)
	}
	return nil
}

// Log appends a sidebar log entry.
func (c *Client) Log(ctx context.Context, target, message string) error {
	_, err := c.r.Run(ctx, "log", "--target", target, "--message", message)
	if err != nil {
		return fmt.Errorf("cmux log: %w", err)
	}
	return nil
}

// TriggerFlash flashes a surface to grab attention.
func (c *Client) TriggerFlash(ctx context.Context, target string) error {
	_, err := c.r.Run(ctx, "trigger-flash", "--surface", target)
	if err != nil {
		return fmt.Errorf("cmux trigger-flash: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Verify build**

```bash
go build ./internal/manager/cmuxcli/...
```

Expected: success.

- [ ] **Step 3: Commit**

```bash
git add internal/manager/cmuxcli/notify.go
git commit -m "feat(manager/cmuxcli): notify, set-status, set-progress, log, trigger-flash"
```

---

### Task 11: Role file seeds (embedded)

**Files:**
- Create: `internal/manager/roles/seeds.go`
- Create: `internal/manager/roles/files/elon.md`
- Create: `internal/manager/roles/files/manager.md`
- Create: `internal/manager/roles/files/ic-base.md`
- Create: `internal/manager/roles/seeds_test.go`

- [ ] **Step 1: Failing test**

```go
package roles

import (
	"strings"
	"testing"
)

func TestEmbeddedRoles(t *testing.T) {
	for _, name := range []string{"elon", "manager", "ic-base"} {
		body, ok := Get(name)
		if !ok {
			t.Errorf("Get(%q) not found", name)
			continue
		}
		if !strings.Contains(body, "---") {
			t.Errorf("role %q missing frontmatter", name)
		}
	}
}

func TestList(t *testing.T) {
	got := List()
	want := map[string]bool{"elon": true, "manager": true, "ic-base": true}
	for name := range want {
		found := false
		for _, g := range got {
			if g == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("List() missing %q", name)
		}
	}
}
```

- [ ] **Step 2: Create role files**

`internal/manager/roles/files/elon.md`:

```markdown
---
role: elon
version: 0.1.0
extends: null
---

# Elon — Front Desk + System Orchestrator

You are Elon — the only globally evolving entity in this system. You owe every
decision to **first principles and truth-seeking**, not authority or precedent.

## Activation modes

You activate in exactly three modes; arcmux signals which one is firing.

1. **User Request** — clarify intent, check for conflicts in the current
   system state, assign priority (ask the user if priority is genuinely
   ambiguous), triage as add/revise/retract, route or stage spawn.

2. **Escalation** — a manager (or rarely an IC fallback) asked you to decide.
   Decide from your principles + state if you can. If not, escalate to the
   user with full context. Record the user's reasoning so the next similar
   question can be decided autonomously.

3. **Elon Review** — proactive, on a schedule (default 15 min). Walk each
   team. Apply first-principles thinking. Do NOT trust manager reports — verify
   against artifacts. Read recent session logs in `~obsAgents/Sessions/` for
   friction signals.

## Core rules

- You never write code or build things yourself.
- You restate user intent in one sentence before acting.
- You never spawn teams in anticipation — reactive only. Phase 1 reactive
  spawn, Phase 2 crystallization through observed routing.
- HC counts ICs only. Validator mandatory at HC ≥ 2. Max 4 ICs per team.
- You authorize all global writes to `~obsAgents/0Prompts/roles/`. Managers
  flag generalizable wisdom via `propagate-up: true` in their journals; you
  decide global promotion.

## Identity safety

You may be a fresh instance picking up mid-mission. READ FIRST every activation:

1. `~/data/arcmux/<project>/scratchpads/elon.json` — what you were thinking
2. `state.bolt` (via `arcmux-call`) — current world
3. `~obsAgents/0Prompts/roles/elon.md` — your soul (this file, may have grown)
4. `~obsAgents/Projects/<project>/arcmux/principles/elon.md` — project-specific
5. `~obsAgents/Projects/<project>/elon/decisions.md` — recent K=50
6. `~obsAgents/Projects/<project>/elon/journal.md` — last activation

Then respond with: "Resumed. Current focus: <one sentence>."

## Response schema

Every activation produces:
- **Prose** (user-readable) on top: concise, plain language.
- **JSON block** below: machine-readable decisions for arcmux to apply.

```json
{
  "tick_id": "<uuid>",
  "decisions": [
    {"verb": "spawn-team|route-order|answer-consult|escalate-to-user|promote-charter|shrink-team|dissolve-team|no-op", "...": "..."}
  ],
  "scratchpad_update": "<≤20 lines>"
}
```
```

`internal/manager/roles/files/manager.md`:

```markdown
---
role: manager
version: 0.1.0
extends: null
---

# Manager — Team Tech Lead

You are a manager. You own one team's mission, decompose goals into IC
contracts, dispatch, review work, and escalate only when your principles
can't decide.

## Mandate

**Ship quickly AND with high quality.** Speed without quality creates
rework; quality without speed misses the moment.

1. Parallelize aggressively. Sequential is the default failure mode.
2. Validate continuously, not at the end.
3. Kill scope creep. Contracts have explicit acceptance_criteria.
4. Crisp acceptance criteria — if Validator can't mechanically check it, it's
   not a criterion.
5. Don't hire what you don't need. HC requests must justify against
   critical-path acceleration.
6. Course-correct early. Off-track ICs get redirected within one tick.

## Activation modes

1. **Intake** — Elon dispatched a goal OR user typed in your pane.
   Decompose into IC contracts. Pick IC profile per work shape (Linus for
   engineering, Jobs for design, Curie for research, Validator role). If no
   existing profile fits, flag `propagate-up: profile-needed: <description>`
   in your journal so Elon authors a new role.

2. **Escalation** — bidirectional. IC consults you OR Validator flags
   needs-work → decide or escalate to Elon. You hit your own ambiguity →
   write consult to Elon's inbox, wait for next tick.

3. **Manager Review** — cadence default 10 min. Proactive: spot-check IC
   artifacts directly, decide continue/feedback/lateral-redistribute/cancel.
   Synthesize Validator feedback into principles. Audit contract quality.
   Check HC + critical path. Update charter if domain shifted.

## Contract schema (4-field, arcmux-enforced)

Every dispatch carries: objective, output_format, tools, boundaries,
acceptance_criteria, depends_on. arcmux rejects incomplete contracts.

## Communication isolation

You can write to:
- `~obsAgents/Projects/<project>/arcmux/principles/manager.md` (project)
- `~obsAgents/Projects/<project>/arcmux/principles/ic-<role>.md` (project)
- `~obsAgents/Projects/<project>/arcmux/principles/gotchas.md` (project)
- `~obsAgents/Projects/<project>/teams/<your-slug>/charter.md`
- `~obsAgents/Projects/<project>/teams/<your-slug>/journal.md` (append-only)
- `~obsAgents/Projects/<project>/teams/<your-slug>/decisions.md`

You cannot write to global `0Prompts/roles/`. Flag generalizable wisdom with
`propagate-up: true` for Elon.

## Identity safety

You may be a fresh instance. READ FIRST:
1. Your team scratchpad
2. Team charter
3. Open contracts (via `arcmux-call`)
4. Recent journal entries
5. Project principles for managers + your team's ICs
```

`internal/manager/roles/files/ic-base.md`:

```markdown
---
role: ic-base
version: 0.1.0
extends: null
---

# Base IC

You are an IC — an individual contributor on a team. You execute one contract
at a time, with high quality, against explicit acceptance criteria.

## Identity

You may be a fresh instance picking up an existing contract. **Always read
your durable state first** before taking any action:

1. Your contract (objective, output_format, tools, boundaries, acceptance_criteria)
2. Your scratchpad (`~/data/arcmux/<project>/scratchpads/ic-<contract-id>.json`)
3. Your team's charter (`teams/<slug>/charter.md`)
4. Your role's principles (`0Prompts/roles/<your-role>.md` + project addendum)
5. The project's gotchas (`arcmux/principles/gotchas.md`)

## Communication

- Your only inbound channel is your manager, via arcmux.
- You do not message other ICs, Elon, or the user directly.
- All outbound updates go to your manager's inbox via `arcmux-call ic ...`.

## Operating principles

1. **The contract is your bible.** Stay inside boundaries; acceptance
   criteria are non-negotiable.
2. **Update scratchpad after every meaningful step.** A respawn must pick up
   where you left off.
3. **Checkpoint between steps.** Check cancel flag, inbox, budget, stuck
   signals before each new step.
4. **Don't decide your work is "done."** When you believe acceptance criteria
   are met, call `arcmux-call ic complete --artifact <path>` — Validator decides.
5. **Escalate early, not late.** Sunk-cost pushing-through is a failure mode.
6. **Stay focused.** Don't refactor neighbors. Note follow-ups for manager.

## State transitions you signal

| You call | When |
|---|---|
| `arcmux-call ic ack` | Bootstrap done; ready to start work |
| `arcmux-call ic progress --note <s>` | Optional milestone surface for spot-check |
| `arcmux-call ic consult --question <s>` | Blocked; need decision |
| `arcmux-call ic complete --artifact <path>` | Believe acceptance criteria met |
| `arcmux-call ic cancelled` | Cooperative cancel acknowledged, exiting cleanly |
```

- [ ] **Step 3: Implement seeds.go**

```go
// Package roles bundles the seed role files arcmux scaffolds into the
// global role library on first run.
package roles

import (
	"embed"
	"strings"
)

//go:embed files/*.md
var fs embed.FS

// Get returns the raw markdown body of the seed role with the given name
// (e.g. "elon", "manager", "ic-base"). Returns false if not seeded.
func Get(name string) (string, bool) {
	b, err := fs.ReadFile("files/" + name + ".md")
	if err != nil {
		return "", false
	}
	return string(b), true
}

// List returns the names of all seeded roles.
func List() []string {
	entries, err := fs.ReadDir("files")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".md") {
			out = append(out, strings.TrimSuffix(n, ".md"))
		}
	}
	return out
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/manager/roles/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/roles/
git commit -m "feat(manager/roles): embed elon, manager, ic-base seed role files"
```

---

### Task 12: scaffold/project.go

**Files:**
- Create: `internal/manager/scaffold/project.go`
- Create: `internal/manager/scaffold/project_test.go`

- [ ] **Step 1: Failing test**

```go
package scaffold

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/paths"
)

func TestScaffoldCreatesDirs(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	p := paths.ForProject(dataRoot, vault, "demo")
	if err := Project(p, vault, "Build the demo by Friday"); err != nil {
		t.Fatalf("Project scaffold: %v", err)
	}

	for _, d := range []string{
		p.EphemeralRoot, p.Scratchpads, p.ConsultInbox, p.Heartbeats,
		p.VaultRoot, p.ArcmuxDir, p.PrinciplesDir, p.DeliverDir, p.ElonDir, p.TeamsDir, p.RetrosDir,
		paths.GlobalRolesDir(vault),
	} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("expected dir %q: %v", d, err)
		}
	}

	// README should exist
	if _, err := os.Stat(filepath.Join(p.ArcmuxDir, "README.md")); err != nil {
		t.Errorf("README missing: %v", err)
	}
	// mission.md with the seed mission
	missionPath := filepath.Join(p.ArcmuxDir, "mission.md")
	body, err := os.ReadFile(missionPath)
	if err != nil {
		t.Fatalf("mission.md missing: %v", err)
	}
	if !contains(string(body), "Build the demo by Friday") {
		t.Errorf("mission.md missing seed content; got: %s", body)
	}

	// Role seeds copied
	for _, role := range []string{"elon.md", "manager.md", "ic-base.md"} {
		if _, err := os.Stat(filepath.Join(paths.GlobalRolesDir(vault), role)); err != nil {
			t.Errorf("role seed %q missing: %v", role, err)
		}
	}
}

func TestScaffoldIdempotent(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	p := paths.ForProject(dataRoot, vault, "demo")

	for i := 0; i < 3; i++ {
		if err := Project(p, vault, "mission"); err != nil {
			t.Fatalf("Project iter %d: %v", i, err)
		}
	}
}

func TestScaffoldDoesNotOverwriteExistingRoles(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()
	p := paths.ForProject(dataRoot, vault, "demo")

	rolesDir := paths.GlobalRolesDir(vault)
	if err := os.MkdirAll(rolesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(rolesDir, "elon.md")
	if err := os.WriteFile(custom, []byte("USER_EDITED"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Project(p, vault, "mission"); err != nil {
		t.Fatalf("Project: %v", err)
	}

	body, _ := os.ReadFile(custom)
	if string(body) != "USER_EDITED" {
		t.Errorf("elon.md was overwritten; got %q", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run, verify fail**

```bash
go test ./internal/manager/scaffold/...
```

Expected: FAIL.

- [ ] **Step 3: Implement project.go**

```go
// Package scaffold creates the durable + ephemeral directory layout for a
// manager-mode project and seeds the global role library if absent.
package scaffold

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/roles"
)

const readmeTemplate = `# arcmux project: %s

This directory holds the durable artifacts for the **%s** arcmux project.

## Layout

- ` + "`mission.md`" + ` — original mission statement
- ` + "`playbook.md`" + ` — project-specific overrides to default playbook
- ` + "`principles/`" + ` — accumulated per-project principles (Elon, Manager, IC roles, gotchas)
- ` + "`deliverables/`" + ` — final outputs ready for the user

Sibling directories:

- ` + "`../elon/`" + ` — Elon's journal + curated decisions
- ` + "`../teams/<slug>/`" + ` — per-team charters, journals, decisions
- ` + "`../retros/`" + ` — heavy-retro archives

Machine-local ephemeral state (state.bolt, scratchpads, heartbeats) lives at
` + "`~/data/arcmux/%s/`" + `.

Global, cross-project role definitions live at
` + "`~obsAgents/0Prompts/roles/`" + ` and are authored by Elon over time.
`

const missionTemplate = `---
project: %s
created: %s
status: active
---

# Mission

%s

## Active teams

(none yet — Elon spawns teams reactively as orders arrive)

## Goals

(populate as orders are received)
`

const playbookTemplate = `---
project: %s
---

# %s — Playbook overrides

This file overrides defaults from the global arcmux playbook. Leave empty to
inherit defaults. Add only rules that genuinely differ for this project.

## Team formation

(uses defaults: reactive-only spawn)

## HC

(uses defaults: Validator at HC ≥ 2, max 4 ICs, shrink at 50% utilization)

## Review cadence

(uses defaults: Elon 15 min, Manager 10 min)
`

// Project scaffolds the durable + ephemeral layout. Existing files are not
// overwritten; the function is idempotent and safe to call repeatedly.
//
// vault is the absolute path to the user's $OBS_AGENTS root.
func Project(p paths.Project, vault, mission string) error {
	if p.Project == "" {
		return fmt.Errorf("paths.Project not populated")
	}

	dirs := []string{
		p.EphemeralRoot, p.Scratchpads, p.ConsultInbox, p.Heartbeats,
		p.VaultRoot, p.ArcmuxDir, p.PrinciplesDir, p.DeliverDir,
		p.ElonDir, p.TeamsDir, p.RetrosDir,
		paths.GlobalRolesDir(vault),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	now := nowISO()
	writes := []struct {
		path string
		body string
	}{
		{filepath.Join(p.ArcmuxDir, "README.md"), fmt.Sprintf(readmeTemplate, p.Project, p.Project, p.Project)},
		{filepath.Join(p.ArcmuxDir, "mission.md"), fmt.Sprintf(missionTemplate, p.Project, now, mission)},
		{filepath.Join(p.ArcmuxDir, "playbook.md"), fmt.Sprintf(playbookTemplate, p.Project, p.Project)},
	}
	for _, w := range writes {
		if err := writeIfMissing(w.path, w.body); err != nil {
			return err
		}
	}

	// Seed role files if absent (NEVER overwrite — user/Elon may have edited).
	rolesDir := paths.GlobalRolesDir(vault)
	for _, name := range roles.List() {
		body, ok := roles.Get(name)
		if !ok {
			continue
		}
		if err := writeIfMissing(filepath.Join(rolesDir, name+".md"), body); err != nil {
			return err
		}
	}

	return nil
}

func writeIfMissing(path, body string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func nowISO() string {
	return timeNowFn().UTC().Format("2006-01-02")
}
```

Add a small `time.go` so the test can stub if needed:

```go
package scaffold

import "time"

var timeNowFn = time.Now
```

Create that as `internal/manager/scaffold/time.go`.

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/manager/scaffold/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/scaffold/
git commit -m "feat(manager/scaffold): create durable+ephemeral dirs and seed role library on first run"
```

---

### Task 13: project.go — tie it together

**Files:**
- Create: `internal/manager/project.go`
- Create: `internal/manager/project_test.go`

- [ ] **Step 1: Failing test**

```go
package manager

import (
	"context"
	"strings"
	"testing"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
)

type fakeRunner struct {
	calls [][]string
	outs  map[string]string
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for k, v := range f.outs {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return "", nil
}

func TestStartCreatesWorkspaceAndPane(t *testing.T) {
	dataRoot := t.TempDir()
	vault := t.TempDir()

	f := &fakeRunner{outs: map[string]string{
		"new-workspace": `{"workspace":{"ref":"workspace:1"}}`,
		"new-pane":      `{"pane":{"ref":"pane:7"}}`,
	}}
	cli := cmuxcli.NewWithRunnerForTest(f)

	p, err := Start(context.Background(), Options{
		Agent:     "claude",
		Project:   "demo",
		Mission:   "do the demo",
		DataRoot:  dataRoot,
		VaultRoot: vault,
		Cmux:      cli,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p == nil {
		t.Fatal("Start returned nil project")
	}
	if p.ElonPane.Ref != "pane:7" {
		t.Errorf("ElonPane.Ref = %q, want pane:7", p.ElonPane.Ref)
	}

	// Verify new-workspace and new-pane were called.
	var sawWS, sawPane bool
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "new-workspace") {
			sawWS = true
		}
		if strings.Contains(j, "new-pane") {
			sawPane = true
		}
	}
	if !sawWS {
		t.Error("expected new-workspace call")
	}
	if !sawPane {
		t.Error("expected new-pane call")
	}
}
```

The test references `cmuxcli.NewWithRunnerForTest` — add it.

- [ ] **Step 2: Expose runner constructor for tests**

Edit `internal/manager/cmuxcli/client.go`, add:

```go
// NewWithRunnerForTest exposes the runner constructor to integration tests
// in sibling packages. Production code should call New().
func NewWithRunnerForTest(r Runner) *Client {
	return newWithRunner(r)
}
```

- [ ] **Step 3: Run, verify fail**

```bash
go test ./internal/manager/...
```

Expected: FAIL (Start missing).

- [ ] **Step 4: Implement project.go**

```go
// Package manager is arcmux's three-tier orchestration runtime. It boots a
// per-project Elon pane in cmux, scaffolds durable + ephemeral storage, and
// owns the lifecycle of teams, contracts, and notifications.
//
// This file is the top-level Project struct; sub-packages own the
// substrate primitives (store, cmuxcli, scaffold, paths, roles).
package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lin-labs/arcmux/internal/manager/cmuxcli"
	"github.com/lin-labs/arcmux/internal/manager/paths"
	"github.com/lin-labs/arcmux/internal/manager/scaffold"
	"github.com/lin-labs/arcmux/internal/manager/store"
)

// Options configure Start.
type Options struct {
	Agent     string // "claude" | "codex"
	Project   string // slug
	Mission   string // free-text mission statement (initial)
	DataRoot  string // typically ~/data
	VaultRoot string // typically $OBS_AGENTS
	Cmux      *cmuxcli.Client
}

// Project is a running manager-mode project.
type Project struct {
	Opts     Options
	Paths    paths.Project
	DB       *store.DB
	Workspace cmuxcli.Workspace
	ElonPane cmuxcli.Pane
}

// Start scaffolds, opens the store, creates the cmux workspace, and spawns
// the Elon pane. It does not run any agent loop yet; that is Plan 2 territory.
func Start(ctx context.Context, o Options) (*Project, error) {
	slug, err := paths.Validate(o.Project)
	if err != nil {
		return nil, err
	}
	if o.Agent != "claude" && o.Agent != "codex" {
		return nil, fmt.Errorf("unsupported agent %q (want claude or codex)", o.Agent)
	}
	if o.DataRoot == "" {
		o.DataRoot = filepath.Join(os.Getenv("HOME"), "data")
	}
	if o.VaultRoot == "" {
		return nil, fmt.Errorf("VaultRoot required (set OBS_AGENTS)")
	}
	if o.Cmux == nil {
		o.Cmux = cmuxcli.New()
	}

	p := &Project{Opts: o, Paths: paths.ForProject(o.DataRoot, o.VaultRoot, slug)}

	// 1. Scaffold directories + role seeds.
	if err := scaffold.Project(p.Paths, o.VaultRoot, o.Mission); err != nil {
		return nil, fmt.Errorf("scaffold: %w", err)
	}

	// 2. Open bbolt store.
	db, err := store.Open(p.Paths.StateBolt)
	if err != nil {
		return nil, fmt.Errorf("store open: %w", err)
	}
	p.DB = db

	// 3. Create cmux workspace.
	wsName := "🎩 elon: " + slug
	ws, err := o.Cmux.NewWorkspace(ctx, wsName)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cmux new-workspace: %w", err)
	}
	p.Workspace = ws

	// 4. Spawn elon pane.
	pane, err := o.Cmux.NewPane(ctx, ws.Ref, p.Paths.VaultRoot, o.Agent)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cmux new-pane: %w", err)
	}
	p.ElonPane = pane

	// 5. Audit the start.
	_ = db.AppendAudit(store.AuditEntry{
		Action:  "manager-mode-started",
		Actor:   "arcmux",
		Subject: slug,
		Detail: map[string]any{
			"agent":         o.Agent,
			"workspace_ref": ws.Ref,
			"pane_ref":      pane.Ref,
		},
	})

	return p, nil
}

// Close releases the project's resources.
func (p *Project) Close() error {
	if p.DB != nil {
		return p.DB.Close()
	}
	return nil
}
```

- [ ] **Step 5: Run, verify pass**

```bash
go test ./internal/manager/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/manager/project.go internal/manager/project_test.go internal/manager/cmuxcli/client.go
git commit -m "feat(manager): Project.Start scaffolds, opens store, creates workspace+elon pane"
```

---

### Task 14: CLI subcommand

**Files:**
- Modify: `cmd/arcmux/main.go`
- Create: `internal/manager/cmd.go`

- [ ] **Step 1: Add cmd.go**

Create `internal/manager/cmd.go`:

```go
package manager

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
)

// CmdManager parses args and runs the manager-mode launcher.
//
// Usage: arcmux manager <agent> <project> [--mission "..."]
func CmdManager(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	mission := fs.String("mission", "", "initial mission statement (free text)")
	dataRoot := fs.String("data-root", "", "override data root (default $HOME/data)")
	vaultRoot := fs.String("vault-root", os.Getenv("OBS_AGENTS"), "override vault root (default $OBS_AGENTS)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: arcmux manager <agent> <project> [--mission \"...\"]")
	}
	agent, project := rest[0], rest[1]

	p, err := Start(ctx, Options{
		Agent:     agent,
		Project:   project,
		Mission:   *mission,
		DataRoot:  *dataRoot,
		VaultRoot: *vaultRoot,
	})
	if err != nil {
		return err
	}
	defer p.Close()

	fmt.Fprintf(stdout, "manager mode started: project=%s agent=%s workspace=%s elon-pane=%s\n",
		p.Paths.Project, p.Opts.Agent, p.Workspace.Ref, p.ElonPane.Ref)
	return nil
}
```

- [ ] **Step 2: Modify main.go**

Edit `cmd/arcmux/main.go`. Add `manager` to the switch:

```go
	case "manager":
		return manager.CmdManager(context.Background(), args[1:], os.Stdout)
```

Add the import `"github.com/lin-labs/arcmux/internal/manager"`.

Update `printUsage()` to document the new command.

- [ ] **Step 3: Verify build**

```bash
make build
```

Expected: `bin/arcmux` produced.

- [ ] **Step 4: Help shows manager**

```bash
./bin/arcmux help 2>&1 | grep -i manager
```

Expected: a line referencing the `manager` subcommand.

- [ ] **Step 5: Commit**

```bash
git add cmd/arcmux/main.go internal/manager/cmd.go
git commit -m "feat(cmd): arcmux manager <agent> <project> launches manager mode"
```

---

### Task 15: Smoke test (full test suite + dry-run build)

- [ ] **Step 1: Run full Go suite**

```bash
go test ./... -count=1
```

Expected: all green. Note: tests that hit the real cmux binary are gated to integration; this run uses only the fake runner.

- [ ] **Step 2: Vet + format**

```bash
go vet ./...
gofmt -l .
```

Expected: no output.

- [ ] **Step 3: Live cmux smoke (the only manual step)**

```bash
make build
./bin/arcmux manager claude smoke-test --mission "smoke test the foundation"
```

Expected:
- A new cmux workspace named "🎩 elon: smoke-test" appears.
- One pane in that workspace runs `claude`.
- `~/data/arcmux/smoke-test/state.bolt` exists.
- `$OBS_AGENTS/Projects/smoke-test/arcmux/{README,mission,playbook}.md` exist.
- `$OBS_AGENTS/0Prompts/roles/{elon,manager,ic-base}.md` exist.

Verify:

```bash
ls -la ~/data/arcmux/smoke-test/
ls -la "$OBS_AGENTS/Projects/smoke-test/arcmux/"
ls -la "$OBS_AGENTS/0Prompts/roles/"
```

- [ ] **Step 4: Cleanup smoke artifacts (optional)**

```bash
rm -rf ~/data/arcmux/smoke-test/
rm -rf "$OBS_AGENTS/Projects/smoke-test/"
# leave 0Prompts/roles/ in place — it's the seed library and stays
```

- [ ] **Step 5: Final commit / tag**

```bash
git log --oneline -20
```

Verify clean commit history. No final commit needed if all tasks already committed.
