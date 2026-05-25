package store

import (
	"errors"
	"testing"
	"time"
)

func TestTeamPutGet(t *testing.T) {
	db := openTestDB(t)

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

	_, err := db.GetTeam("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListTeams(t *testing.T) {
	db := openTestDB(t)

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
