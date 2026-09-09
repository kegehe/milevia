package agent

import "testing"

func TestCredentialSerializationRoundTrip(t *testing.T) {
	want := storedCredential{InstanceID: "pc-123", AgentToken: "mva_secret"}
	got, err := parseStoredCredential(serializeCredential(want))
	if err != nil {
		t.Fatalf("parse serialized credential: %v", err)
	}
	if got != want {
		t.Fatalf("credential = %#v, want %#v", got, want)
	}
}

func TestParseStoredCredentialRejectsIncompleteData(t *testing.T) {
	if _, err := parseStoredCredential([]byte("MILEVIA_INSTANCE_ID=pc-only\n")); err == nil {
		t.Fatal("incomplete credential was accepted")
	}
}
