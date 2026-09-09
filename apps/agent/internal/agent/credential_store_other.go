//go:build !windows

package agent

import (
	"os"
	"path/filepath"
)

// Desktop releases currently target Windows. Keep a restricted-file fallback
// for development and future non-Windows Agent builds.
func loadCredentialFile(path string) (storedCredential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return storedCredential{}, err
	}
	return parseStoredCredential(data)
}

func saveCredentialFile(path string, credential storedCredential) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, serializeCredential(credential), 0o600)
}
