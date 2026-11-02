package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestInteractionRemoteOperationWhitelistIsExplicit(t *testing.T) {
	operations := (&Server{}).remoteInteractionOperations()
	if len(operations) != 4 {
		t.Fatalf("interaction operation count = %d, want 4", len(operations))
	}
	ask, ok := operations["interaction.answerAsk"]
	if !ok || ask.Method != http.MethodPost || ask.Scope != relayScopeAPI || ask.Path != "/api/asks/{askID}" || ask.Query || ask.Handle == nil {
		t.Fatalf("interaction.answerAsk = %#v", ask)
	}
	approval, ok := operations["interaction.resolveApproval"]
	if !ok || approval.Method != http.MethodPost || approval.Scope != relayScopeAPI || approval.Path != "/api/approvals/{approvalID}" || approval.Handle == nil {
		t.Fatalf("interaction.resolveApproval = %#v", approval)
	}
	handoff, ok := operations["handoff.status"]
	if !ok || handoff.Method != http.MethodGet || handoff.Scope != relayScopeAPI || !handoff.Query || handoff.Handle == nil {
		t.Fatalf("handoff.status = %#v", handoff)
	}
	profiles, ok := operations["runner.profileChoices"]
	if !ok || profiles.Method != http.MethodGet || profiles.Scope != relayScopeAPI || profiles.Handle == nil {
		t.Fatalf("runner.profileChoices = %#v", profiles)
	}
}

func TestRelayTargetKeepsAPIOperationsOffTheProjectPrefix(t *testing.T) {
	askID := "11111111-1111-1111-1111-111111111111"
	target, body, pathParams, err := relayTarget(
		(&Server{}).remoteInteractionOperations()["interaction.answerAsk"],
		"project-1", "conversation-1",
		json.RawMessage(`{"askID":"`+askID+`","action":"skip","conversationId":"forged"}`),
	)
	if err != nil {
		t.Fatalf("relayTarget: %v", err)
	}
	if target != "/api/asks/"+askID {
		t.Fatalf("target = %q, want the ask route without a project prefix", target)
	}
	if pathParams["askID"] != askID {
		t.Fatalf("pathParams = %v", pathParams)
	}
	if strings.Contains(string(body), "conversationId") || strings.Contains(string(body), "forged") {
		t.Fatalf("body = %s, conversationId must be stripped", body)
	}
	if !strings.Contains(string(body), `"action":"skip"`) {
		t.Fatalf("body = %s, want the skip action", body)
	}
}

func TestRelayTargetRejectsAForgedRunnerID(t *testing.T) {
	_, _, _, err := relayTarget(
		(&Server{}).remoteInteractionOperations()["runner.profileChoices"],
		"project-1", "",
		json.RawMessage(`{"runnerID":"../secrets"}`),
	)
	if err == nil {
		t.Fatal("expected runnerID with a path separator to be rejected")
	}
}
