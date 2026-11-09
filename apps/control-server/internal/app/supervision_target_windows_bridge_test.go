package app

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetWindowsBridgeKeepsPathDataOutOfScript(t *testing.T) {
	home := `C:\test's 中文 ; $value\target`
	script, err := supervisionWindowsTargetScript(supervisionWindowsTargetInstall, strings.Repeat("a", 64), home)
	if err != nil || strings.Contains(script, home) || !strings.Contains(script, "OpenStandardInput") {
		t.Fatal("path data became PowerShell code", err)
	}
	encoded := base64.StdEncoding.EncodeToString(mustJSON(map[string]string{"digest": strings.Repeat("a", 64), "home": home, "mode": "supervision-target"}))
	if !strings.Contains(script, encoded) {
		t.Fatal("encoded request missing")
	}
	var decoded map[string]string
	raw, _ := base64.StdEncoding.DecodeString(encoded)
	if json.Unmarshal(raw, &decoded) != nil || decoded["home"] != home {
		t.Fatal("path bytes changed")
	}
	for _, path := range []string{"line\nfeed", "nul\x00byte", `C:\bad"path`} {
		if _, err := supervisionWindowsTargetScript("", strings.Repeat("a", 64), path); err != targetexec.ErrConflict {
			t.Fatal("invalid path accepted", err)
		}
	}
	for _, digest := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 63) + ";"} {
		if _, err := supervisionWindowsTargetScript("", digest, home); err != targetexec.ErrConflict {
			t.Fatal("invalid content address accepted", err)
		}
	}
}
