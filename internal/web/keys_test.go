package web

import (
	"encoding/json"
	"os"
	"testing"
)

// 明文密钥与哈希一起持久化：控制台可以随时重新显示并复制完整密钥。
func TestAPIKeyRawPersistedAndReloadable(t *testing.T) {
	path := t.TempDir() + "/api-keys.json"
	store := newAPIKeyStore(path)
	_, raw, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("create must return the raw key")
	}
	if store.Keys[0].Raw != raw {
		t.Fatalf("raw not kept on record: %q", store.Keys[0].Raw)
	}
	if store.Keys[0].Hash == "" {
		t.Fatal("hash must be stored for validation")
	}
	// 重新打开（模拟重启）后磁盘上仍有明文，可再次显示。
	re := newAPIKeyStore(path)
	if b, e := os.ReadFile(path); e != nil || json.Unmarshal(b, re) != nil {
		t.Fatalf("reload failed: %v", e)
	}
	if got := re.Keys[0].Raw; got != raw {
		t.Fatalf("raw not persisted to disk: %q", got)
	}
	// 旧版本已存在的明文（无哈希）在加载时补写哈希，明文保留。
	legacy := `{"keys":[{"id":"legacy","name":"old","prefix":"m365_abcd","raw":"m365_secret_value","hash":"","createdAt":"2025-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_API_KEYS", path)
	store2 := openAPIKeys()
	if len(store2.Keys) != 1 {
		t.Fatalf("legacy keys=%d, want 1", len(store2.Keys))
	}
	if got := store2.Keys[0].Raw; got != "m365_secret_value" {
		t.Fatalf("legacy raw lost after migration: %q", got)
	}
	if store2.Keys[0].Hash != keyHash("m365_secret_value") {
		t.Fatalf("legacy hash not backfilled: %q", store2.Keys[0].Hash)
	}
	// list 不下发 hash，但下发 raw 供控制台重复显示。
	out := store2.list()
	if len(out) != 1 || out[0].Raw != "m365_secret_value" || out[0].Hash != "" {
		t.Fatalf("list must expose raw but not hash: %#v", out)
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

// 旧 hash-only 时代的密钥（只有 hash、没有明文）在加载时被轮换为明文密钥，
// 旧的 hash 保留在 legacyHash 中继续可用，控制台能重新显示完整密钥。
func TestAPIKeyLegacyHashOnlyRotatedToPlaintext(t *testing.T) {
	path := t.TempDir() + "/api-keys.json"
	legacyHash := keyHash("m365_old_plaintext_that_was_never_stored")
	legacy := `{"keys":[{"id":"legacy1","name":"old","prefix":"m365_old_plain","hash":"` + legacyHash + `","createdAt":"2025-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_API_KEYS", path)
	s := openAPIKeys()
	if len(s.Keys) != 1 {
		t.Fatalf("keys=%d, want 1", len(s.Keys))
	}
	k := s.Keys[0]
	if k.Raw == "" {
		t.Fatal("legacy key was not rotated to a plaintext raw")
	}
	if k.Hash != keyHash(k.Raw) {
		t.Fatalf("hash does not match rotated raw: %q", k.Hash)
	}
	if k.LegacyHash != legacyHash {
		t.Fatalf("legacy hash not preserved: got %q want %q", k.LegacyHash, legacyHash)
	}
	// 磁盘上现在有明文，可重复显示。
	re := newAPIKeyStore(path)
	if b, e := os.ReadFile(path); e != nil || json.Unmarshal(b, re) != nil {
		t.Fatalf("reload failed: %v", e)
	}
	if got := re.Keys[0].Raw; got == "" {
		t.Fatal("rotated plaintext not persisted to disk")
	}
	// list 不下发 hash/legacyHash，但下发 raw。
	out := s.list()
	if len(out) != 1 || out[0].Raw == "" || out[0].Hash != "" || out[0].LegacyHash != "" {
		t.Fatalf("list must expose raw but not hashes: %#v", out)
	}
	// 旧值仍然有效（兼容既有客户端），新值也有效。
	if !s.valid("m365_old_plaintext_that_was_never_stored") {
		t.Fatal("legacy raw value must remain valid through legacyHash")
	}
	if !s.valid(k.Raw) {
		t.Fatal("rotated raw must be valid")
	}
	if s.valid("m365_totally_wrong_key") {
		t.Fatal("unknown key must not validate")
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
