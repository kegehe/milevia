package targetexec

import "context"

// RuntimeCredentials travels only on the authenticated owner pipe and in the
// running goroutine's context. Never add it to Input, Execution, Facts or Events.
type RuntimeCredentials struct {
	TargetID      string            `json:"targetId,omitempty"`
	RevisionID    string            `json:"revisionId"`
	AgentID       string            `json:"agentId"`
	BaseURL       string            `json:"baseUrl,omitempty"`
	Model         string            `json:"model,omitempty"`
	AuthMode      string            `json:"authMode"`
	ExecutionMode string            `json:"executionMode"`
	Secret        string            `json:"secret,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
}

type runtimeCredentialsKey struct{}

func ExecutionCredentials(ctx context.Context) *RuntimeCredentials {
	value, _ := ctx.Value(runtimeCredentialsKey{}).(*RuntimeCredentials)
	return value
}
