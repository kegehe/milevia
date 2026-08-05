// Command approval-helper bridges Claude Code's hook stdin to Milevia's local
// approval endpoint without requiring sh, curl, or PowerShell.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const maxHookPayload = 8 << 20

func main() {
	controlURL := os.Getenv("AUTO_CONTROL_URL")
	token := os.Getenv("AUTO_APPROVAL_TOKEN")
	if controlURL == "" || token == "" {
		fail("AUTO_CONTROL_URL and AUTO_APPROVAL_TOKEN are required")
	}
	identityHeader, identityValue := "", ""
	if value := os.Getenv("AUTO_APPROVAL_CONVERSATION_ID"); value != "" {
		identityHeader, identityValue = "X-Auto-Conversation-ID", value
	} else if value := os.Getenv("AUTO_APPROVAL_RUN_ID"); value != "" {
		identityHeader, identityValue = "X-Auto-Run-ID", value
	} else {
		fail("an approval conversation or run ID is required")
	}

	payload, err := io.ReadAll(io.LimitReader(os.Stdin, maxHookPayload+1))
	if err != nil {
		fail("read approval request: " + err.Error())
	}
	if len(payload) > maxHookPayload {
		fail("approval request is too large")
	}
	req, err := http.NewRequest(http.MethodPost, controlURL+"/api/internal/approvals/wait", bytes.NewReader(payload))
	if err != nil {
		fail("create approval request: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identityHeader, identityValue)
	req.Header.Set("X-Auto-Approval-Token", token)
	client := &http.Client{Timeout: 310 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		fail("submit approval request: " + err.Error())
	}
	defer response.Body.Close()
	_, _ = io.Copy(os.Stdout, io.LimitReader(response.Body, maxHookPayload))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		fail(fmt.Sprintf("approval request failed: status %d", response.StatusCode))
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
