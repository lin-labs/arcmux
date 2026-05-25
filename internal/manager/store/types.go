package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewInboxID returns a sortable id of the form "<unix-ns>-<6-hex>", suitable
// for InboxMsg.ID. The time prefix gives natural chronological ordering in
// logs and tooling; the hex suffix avoids collisions on identical-nano
// writes from independent processes.
func NewInboxID() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:])), nil
}

// Team is a manager-led group of ICs working a domain.
type Team struct {
	ID           string    `json:"id"`
	Vision       string    `json:"vision"`
	State        string    `json:"state"`
	HC           int       `json:"hc"`
	TargetHC     int       `json:"target_hc"`
	WorkspaceRef string    `json:"workspace_ref"`
	ManagerPane  string    `json:"manager_pane"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Contract is the Anthropic 4-field unit of IC work, plus DAG + lifecycle.
type Contract struct {
	ID                 string          `json:"id"`
	Team               string          `json:"team"`
	ICRole             string          `json:"ic_role"`
	Priority           int             `json:"priority"`
	State              string          `json:"state"`
	Objective          string          `json:"objective"`
	OutputFormat       string          `json:"output_format"`
	Tools              []string        `json:"tools"`
	Boundaries         []string        `json:"boundaries"`
	AcceptanceCriteria []string        `json:"acceptance_criteria"`
	DependsOn          []string        `json:"depends_on"`
	ParallelizableWith []string        `json:"parallelizable_with"`
	Capstone           bool            `json:"capstone"`
	Deadline           *time.Time      `json:"deadline,omitempty"`
	Validations        []Validation    `json:"validations"`
	Audit              []ContractAudit `json:"audit"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// Validation is one validator pass on a contract.
type Validation struct {
	Timestamp time.Time `json:"ts"`
	By        string    `json:"by"`
	Verdict   string    `json:"verdict"`
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
	Timestamp time.Time      `json:"ts"`
	Action    string         `json:"action"`
	Actor     string         `json:"actor"`
	Subject   string         `json:"subject"`
	RuleID    string         `json:"rule_id,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// InboxMsg is a queued message awaiting Elon/Manager processing.
type InboxMsg struct {
	ID         string         `json:"id"`
	Verb       string         `json:"verb"`
	From       string         `json:"from"`
	Priority   int            `json:"priority"`
	Body       string         `json:"body"`
	Refs       map[string]any `json:"refs,omitempty"`
	ReceivedAt time.Time      `json:"received_at"`
}

// Slot is one IC headcount slot within a team. Spawned via Plan 5's
// icspawn primitive; the bbolt record is the durable handle that lets a
// respawn locate the pane, the seeded scratchpad, and the IC's bootstrap
// script.
type Slot struct {
	ID             string    `json:"id"`              // unique slot id within project (slug)
	Team           string    `json:"team"`            // owning team slug
	Role           string    `json:"role"`            // specialization (ic-base | linus | jobs | validator | ...)
	Contract       string    `json:"contract"`        // currently-bound contract id (may be reassigned in Plan 6+)
	PaneRef        string    `json:"pane_ref"`        // cmux pane reference (split inside team workspace)
	WorkspaceRef   string    `json:"workspace_ref"`   // same as team.WorkspaceRef
	ScratchpadPath string    `json:"scratchpad_path"` // ~/data/arcmux/<project>/scratchpads/<arcmux_role>.json
	BootstrapPath  string    `json:"bootstrap_path"`  // shell script that exec's the agent CLI
	Agent          string    `json:"agent"`           // "claude" | "codex"
	State          string    `json:"state"`           // active | idle | dissolved
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Valid slot states.
const (
	SlotActive    = "active"
	SlotIdle      = "idle"
	SlotDissolved = "dissolved"
)

// MaxICsPerTeam caps active IC slots per team (spec §10). Includes the
// Validator (mandatory at HC ≥ 2) but excludes the manager.
const MaxICsPerTeam = 4

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
