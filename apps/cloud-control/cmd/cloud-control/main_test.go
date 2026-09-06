package main

import "testing"

func TestAgentTokensFromEnv(t *testing.T) {
	tokens, err := agentTokensFromEnv(`{"pc-a":"secret-a"}`)
	if err != nil || tokens["pc-a"] != "secret-a" {
		t.Fatalf("tokens=%v err=%v", tokens, err)
	}
	if _, err := agentTokensFromEnv(`{"pc-a":""}`); err == nil {
		t.Fatal("empty token was accepted")
	}
	if _, err := agentTokensFromEnv(""); err == nil {
		t.Fatal("empty mapping was accepted")
	}
}
