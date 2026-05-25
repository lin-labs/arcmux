package store

import (
	"testing"
	"time"
)

func TestContractPutGet(t *testing.T) {
	db := openTestDB(t)

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

	teamCs, err := db.ListContractsByTeam("team-a")
	if err != nil {
		t.Fatalf("ListContractsByTeam: %v", err)
	}
	if len(teamCs) != 1 || teamCs[0] != "c-1" {
		t.Errorf("ListContractsByTeam = %v, want [c-1]", teamCs)
	}

	pendingCs, err := db.ListContractsByState(ContractPending)
	if err != nil {
		t.Fatalf("ListContractsByState: %v", err)
	}
	if len(pendingCs) != 1 {
		t.Errorf("pending contracts = %v, want [c-1]", pendingCs)
	}

	children, err := db.ChildrenOf("c-0")
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if len(children) != 1 || children[0] != "c-1" {
		t.Errorf("ChildrenOf(c-0) = %v, want [c-1]", children)
	}

	parents, err := db.ParentsOf("c-1")
	if err != nil {
		t.Fatalf("ParentsOf: %v", err)
	}
	if len(parents) != 1 || parents[0] != "c-0" {
		t.Errorf("ParentsOf(c-1) = %v, want [c-0]", parents)
	}
}

func TestContractTransitionValid(t *testing.T) {
	db := openTestDB(t)

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

	c0 := Contract{ID: "c-0", Team: "team-a", State: ContractPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	c1 := Contract{ID: "c-1", Team: "team-a", State: ContractPending, DependsOn: []string{"c-0"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	_ = db.PutContract(c0)
	_ = db.PutContract(c1)

	// Need to reach Ready first; that itself requires deps. Try direct working.
	if err := db.TransitionContract("c-1", ContractReady, "go", "manager"); err == nil {
		t.Error("expected error transitioning to ready with unmet dep")
	}
}

func TestContractTransitionDepsMet(t *testing.T) {
	db := openTestDB(t)

	c0 := Contract{ID: "c-0", Team: "team-a", State: ContractPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	c1 := Contract{ID: "c-1", Team: "team-a", State: ContractPending, DependsOn: []string{"c-0"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	_ = db.PutContract(c0)
	_ = db.PutContract(c1)

	// Walk c0 through to completed.
	if err := db.TransitionContract("c-0", ContractReady, "", "test"); err != nil {
		t.Fatalf("c-0 → ready: %v", err)
	}
	if err := db.TransitionContract("c-0", ContractWorking, "", "test"); err != nil {
		t.Fatalf("c-0 → working: %v", err)
	}
	if err := db.TransitionContract("c-0", ContractValidating, "", "test"); err != nil {
		t.Fatalf("c-0 → validating: %v", err)
	}
	if err := db.TransitionContract("c-0", ContractCompleted, "", "test"); err != nil {
		t.Fatalf("c-0 → completed: %v", err)
	}

	// Now c-1 should be allowed to go Ready.
	if err := db.TransitionContract("c-1", ContractReady, "deps met", "manager"); err != nil {
		t.Errorf("c-1 → ready with completed deps: %v", err)
	}
}

func TestListContractsFilters(t *testing.T) {
	db := openTestDB(t)

	now := time.Now()
	seeds := []Contract{
		{ID: "alpha-1", Team: "alpha", State: ContractPending, Priority: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "alpha-2", Team: "alpha", State: ContractReady, Priority: 5, CreatedAt: now, UpdatedAt: now},
		{ID: "alpha-3", Team: "alpha", State: ContractPending, Priority: 9, CreatedAt: now, UpdatedAt: now},
		{ID: "beta-1", Team: "beta", State: ContractPending, Priority: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "beta-2", Team: "beta", State: ContractCompleted, Priority: 9, CreatedAt: now, UpdatedAt: now},
	}
	for _, c := range seeds {
		if err := db.PutContract(c); err != nil {
			t.Fatalf("seed %s: %v", c.ID, err)
		}
	}

	cases := []struct {
		name    string
		team    string
		state   string
		wantIDs []string // expected order
	}{
		{"all", "", "", []string{"alpha-3", "beta-2", "alpha-2", "beta-1", "alpha-1"}},
		{"team-alpha", "alpha", "", []string{"alpha-3", "alpha-2", "alpha-1"}},
		{"state-pending", "", ContractPending, []string{"alpha-3", "beta-1", "alpha-1"}},
		{"team-and-state", "alpha", ContractPending, []string{"alpha-3", "alpha-1"}},
		{"team-empty-filter", "ghost", "", nil},
		{"state-empty-filter", "", ContractFailed, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.ListContracts(tc.team, tc.state)
			if err != nil {
				t.Fatalf("ListContracts: %v", err)
			}
			ids := make([]string, len(got))
			for i, c := range got {
				ids[i] = c.ID
			}
			if !equalStrings(ids, tc.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestContractInvalidTransition(t *testing.T) {
	db := openTestDB(t)
	c := Contract{ID: "c-1", Team: "team-a", State: ContractCompleted, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	_ = db.PutContract(c)

	if err := db.TransitionContract("c-1", ContractWorking, "", "test"); err == nil {
		t.Error("expected error transitioning from completed")
	}
}
