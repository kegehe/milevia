package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestClaudeResumeAfterProcessExitPOC is an opt-in release gate for native
// Session reclamation. It uses the operator's installed/login Claude CLI and
// therefore may incur model usage. The two commands run in separate processes;
// the second command must recover the token created by the first one.
func TestClaudeResumeAfterProcessExitPOC(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_CLAUDE_RESUME_POC") != "1" {
		t.Skip("set MILEVIA_RUN_CLAUDE_RESUME_POC=1 to run the installed Claude resume POC")
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("Claude Code is not installed")
	}
	workDir := t.TempDir()
	sessionID := uuid.NewString()
	token := "MILEVIA_RESUME_PROBE_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	first := runClaudeResumePOCTurn(t, claudePath, workDir, "--session-id", sessionID,
		"Remember this exact token for a later question and reply only with ACK: "+token)
	if first.IsError || first.SessionID != sessionID || strings.TrimSpace(first.Result) != "ACK" {
		t.Fatalf("initial Claude turn did not establish the requested session: %#v", first)
	}
	resumed := runClaudeResumePOCTurn(t, claudePath, workDir, "--resume", sessionID,
		"Reply only with the exact token I asked you to remember in the previous turn.")
	if resumed.IsError || resumed.SessionID != sessionID || strings.TrimSpace(resumed.Result) != token {
		t.Fatalf("Claude did not resume the prior process session: %#v", resumed)
	}
}

type claudeResumePOCResult struct {
	IsError   bool   `json:"is_error"`
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
}

func runClaudeResumePOCTurn(t *testing.T, claudePath, workDir, sessionFlag, sessionID, prompt string) claudeResumePOCResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, claudePath, "-p", "--output-format", "json", sessionFlag, sessionID, "--permission-mode", "plan", prompt)
	command.Dir = workDir
	output, err := command.Output()
	if err != nil {
		t.Fatalf("run Claude resume POC: %v\n%s", err, redactAgentText(string(output)))
	}
	var result claudeResumePOCResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Claude resume POC output: %v\n%s", err, redactAgentText(string(output)))
	}
	return result
}
