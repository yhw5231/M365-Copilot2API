package auth

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUpsertAndList(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := store.Upsert(TokenSet{
		AccessToken:  "a",
		RefreshToken: "r",
		Email:        "a@example.com",
		DisplayName:  "A",
		HomeOID:      "oid-1",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if acc.Email != "a@example.com" {
		t.Fatalf("unexpected email: %s", acc.Email)
	}
	list := store.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 account, got %d", len(list))
	}
}

func TestScheduleEnabledPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token := TokenSet{AccessToken: "a", RefreshToken: "r", Email: "a@example.com", HomeOID: "oid-1", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := store.Upsert(token); err != nil {
		t.Fatal(err)
	}
	if !store.ScheduleEnabled("oid-1") {
		t.Fatal("new account scheduling disabled")
	}
	if err := store.SetScheduleEnabled("oid-1", false); err != nil {
		t.Fatal(err)
	}
	if store.ScheduleEnabled("oid-1") {
		t.Fatal("account scheduling still enabled")
	}
	if _, err := store.Upsert(token); err != nil {
		t.Fatal(err)
	}
	if store.ScheduleEnabled("oid-1") {
		t.Fatal("upsert reset scheduling state")
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.ScheduleEnabled("oid-1") {
		t.Fatal("scheduling state was not persisted")
	}
}

func TestMoveToBackRotatesSchedulingQueue(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"oid-1", "oid-2", "oid-3"} {
		_, err := store.Upsert(TokenSet{
			AccessToken: "access-" + id,
			Email:       id + "@example.com",
			HomeOID:     id,
			ExpiresAt:   time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("upsert account %d: %v", i, err)
		}
	}

	if !store.MoveToBack("oid-1") {
		t.Fatal("MoveToBack did not find oid-1")
	}
	got := store.List()
	want := []string{"oid-2", "oid-3", "oid-1"}
	if len(got) != len(want) {
		t.Fatalf("queue length=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("queue[%d]=%s want %s", i, got[i].ID, want[i])
		}
	}

	if store.MoveToBack("missing") {
		t.Fatal("MoveToBack unexpectedly found a missing account")
	}
}
