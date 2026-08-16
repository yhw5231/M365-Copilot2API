package config

import (
	"encoding/json"
	"os"

	"m365-copilot2api/internal/storage"
)

type Account struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName,omitempty"`
	Status      string `json:"status"`
}

type Store struct {
	Accounts []Account `json:"accounts"`
}

func Path() string {
	return storage.Path("M365_CONFIG", "accounts.json")
}

func Load() (Store, error) {
	b, e := os.ReadFile(Path())
	if os.IsNotExist(e) {
		return Store{Accounts: []Account{}}, nil
	}
	if e != nil {
		return Store{}, e
	}
	var s Store
	e = json.Unmarshal(b, &s)
	return s, e
}

func Save(s Store) error {
	p := Path()
	b, e := json.MarshalIndent(s, "", "  ")
	if e != nil {
		return e
	}
	return storage.WriteFileAtomic(p, b, 0o600)
}
