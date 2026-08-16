package web

import "m365-copilot2api/internal/storage"

func envPath(name string) string {
	return storage.EnvPath(name)
}

func defaultDataDir() string {
	return storage.DataDir()
}

func defaultDataPath(name string) string {
	return storage.Path("", name)
}

func configuredPath(env, name string) string {
	return storage.Path(env, name)
}
