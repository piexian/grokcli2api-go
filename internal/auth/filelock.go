package auth

import "path/filepath"

func authLockPath(authPath string) string {
	return filepath.Join(filepath.Dir(authPath), "auth.json.lock")
}
