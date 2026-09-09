package agent

import (
	"fmt"
	"strings"
)

type storedCredential struct {
	InstanceID string
	AgentToken string
}

func parseStoredCredential(data []byte) (storedCredential, error) {
	var credential storedCredential
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "MILEVIA_INSTANCE_ID":
			credential.InstanceID = strings.TrimSpace(value)
		case "MILEVIA_CLOUD_AGENT_TOKEN":
			credential.AgentToken = strings.TrimSpace(value)
		}
	}
	if credential.InstanceID == "" || credential.AgentToken == "" {
		return storedCredential{}, fmt.Errorf("stored credential is incomplete")
	}
	return credential, nil
}

func serializeCredential(credential storedCredential) []byte {
	return []byte(fmt.Sprintf("MILEVIA_INSTANCE_ID=%s\nMILEVIA_CLOUD_AGENT_TOKEN=%s\n", credential.InstanceID, credential.AgentToken))
}
