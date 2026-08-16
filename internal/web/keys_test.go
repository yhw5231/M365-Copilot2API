package web

import (
	"encoding/json"
	"os"
	"testing"
)

// 密钥明文持久化保存，控制台可重复显示完整密钥。
func TestAPIKeyRawPersistedForRepeatedDisplay(t *testing.T) {
	path := t.TempDir() + "/api-keys.json"
	store := newAPIKeyStore(path)
	_, raw, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Keys[0].Raw; got != raw {
		t.Fatalf("stored raw = %q, want %q", got, raw)
	}
	// 重新打开（模拟重启）后明文仍在。
	re := newAPIKeyStore(path)
	if b, e := os.ReadFile(path); e != nil || json.Unmarshal(b, re) != nil {
		t.Fatalf("reload failed: %v", e)
	}
	if got := re.Keys[0].Raw; got != raw {
		t.Fatalf("raw lost after reload: %q", got)
	}
	// list 返回 raw 供控制台显示，但 Hash 永不下发。
	out := re.list()
	if len(out) != 1 || out[0].Raw != raw {
		t.Fatalf("list raw = %q, want %q", out[0].Raw, raw)
	}
	if out[0].Hash != "" {
		t.Fatalf("list must not expose hash")
	}
}

func TestAPIKeyCreateRollsBackWhenPersistenceFails(t *testing.T) {
	store := newAPIKeyStore(t.TempDir())
	if _, _, err := store.create("test"); err == nil {
		t.Fatal("expected persistence error")
	}
	if got := len(store.Keys); got != 0 {
		t.Fatalf("retained %d in-memory keys after failed save", got)
	}
}

func TestAPIKeyRevokeRollsBackWhenPersistenceFails(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	record, _, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	store.Path = t.TempDir()
	revoked, err := store.revoke(record.ID)
	if err == nil || revoked {
		t.Fatalf("revoke=%v err=%v, want persistence failure", revoked, err)
	}
	if store.Keys[0].Revoked {
		t.Fatal("key remained revoked after failed save")
	}
}

func TestAPIKeyDeletePhysicallyRemoves(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	r1, _, err := store.create("one")
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := store.create("two")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.delete(r1.ID)
	if err != nil || !deleted {
		t.Fatalf("delete=%v err=%v", deleted, err)
	}
	for _, k := range store.Keys {
		if k.ID == r1.ID {
			t.Fatal("key still present after delete")
		}
	}
	if len(store.Keys) != 1 || store.Keys[0].ID != r2.ID {
		t.Fatalf("unexpected remaining keys: %+v", store.Keys)
	}
	if deleted, _ := store.delete("no-such-id"); deleted {
		t.Fatal("delete of unknown id should report false")
	}
}

func TestAPIKeyDeleteRollsBackWhenPersistenceFails(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	record, _, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	store.Path = t.TempDir()
	deleted, err := store.delete(record.ID)
	if err == nil || deleted {
		t.Fatalf("delete=%v err=%v, want persistence failure", deleted, err)
	}
	if len(store.Keys) != 1 || store.Keys[0].ID != record.ID {
		t.Fatalf("key not restored after failed delete: %+v", store.Keys)
	}
}
