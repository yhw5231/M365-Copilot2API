package web

import (
	"encoding/json"
	"os"
	"testing"
)

// 安全改进（上游 keys.go）：明文密钥只在创建时返回一次，磁盘仅存哈希。
func TestAPIKeyRawNotPersistedAfterCreate(t *testing.T) {
	path := t.TempDir() + "/api-keys.json"
	store := newAPIKeyStore(path)
	_, raw, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("create must return the raw key once")
	}
	if got := store.Keys[0].Raw; got != "" {
		t.Fatalf("raw persisted to record = %q, want empty", got)
	}
	if store.Keys[0].Hash == "" {
		t.Fatal("hash must be stored for validation")
	}
	// 重新打开（模拟重启）后磁盘上仍无明文。
	re := newAPIKeyStore(path)
	if b, e := os.ReadFile(path); e != nil || json.Unmarshal(b, re) != nil {
		t.Fatalf("reload failed: %v", e)
	}
	if got := re.Keys[0].Raw; got != "" {
		t.Fatalf("raw persisted to disk: %q", got)
	}
	// 旧版本已存在的明文在加载时会被清零，并补写哈希。
	legacy := `{"keys":[{"id":"legacy","name":"old","prefix":"m365_abcd","raw":"m365_secret_value","hash":"","createdAt":"2025-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_API_KEYS", path)
	store2 := openAPIKeys()
	if len(store2.Keys) != 1 {
		t.Fatalf("legacy keys=%d, want 1", len(store2.Keys))
	}
	if got := store2.Keys[0].Raw; got != "" {
		t.Fatalf("legacy raw still present after migration: %q", got)
	}
	if store2.Keys[0].Hash != keyHash("m365_secret_value") {
		t.Fatalf("legacy hash not backfilled: %q", store2.Keys[0].Hash)
	}
	// list 不下发 hash，也不下发 raw。
	out := store2.list()
	if len(out) != 1 || out[0].Raw != "" || out[0].Hash != "" {
		t.Fatalf("list must not expose hash/raw: %#v", out)
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
