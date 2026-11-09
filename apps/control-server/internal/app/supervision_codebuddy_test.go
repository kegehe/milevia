package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSupervisionCodebuddyProtocolRequiresEmptyToolsAndIndependentInitialization(t *testing.T) {
	for _, tc := range []struct {
		name, init string
		ok         bool
	}{
		{"native_without_plugins", `{"type":"system","subtype":"init","session_id":"native","tools":[],"mcp_servers":[]}`, true},
		{"unexpected_tool", `{"type":"system","subtype":"init","tools":["Bash"],"mcp_servers":[]}`, false},
		{"unexpected_mcp", `{"type":"system","subtype":"init","tools":[],"mcp_servers":[{}]}`, false},
		{"unexpected_plugin", `{"type":"system","subtype":"init","tools":[],"mcp_servers":[],"plugins":[{}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := newSupervisionCodebuddyStream(&claudeOutputTestSink{}, cancel)
			_, _ = stream.Write([]byte(tc.init + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"{}"}`))
			stream.flush()
			if (stream.err == nil) != tc.ok || (!tc.ok && ctx.Err() == nil) {
				t.Fatalf("protocol accepted=%t err=%v", tc.ok, stream.err)
			}
		})
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	claude := &supervisionSchedulerStream{sink: &claudeOutputTestSink{}, cancel: cancel}
	_, _ = claude.Write([]byte(`{"type":"system","subtype":"init","tools":[],"mcp_servers":[]}`))
	claude.flush()
	if claude.err == nil {
		t.Fatal("CodeBuddy compatibility weakened Claude plugin gate")
	}
}

func TestSupervisionCodebuddyArgumentsDenyAllKnownBuiltinsAndUseStdin(t *testing.T) {
	request := schedulerCLIRequestForTest()
	request.ReasoningEffort = ""
	args := supervisionCodebuddyArgs(request, supervisionCLIInfo{Entrypoint: "/trusted/entry"})
	if !containsArguments(args, "--tools", "MileviaNoExecutionTool") || !containsArguments(args, "--setting-sources", "") || !containsArguments(args, "--permission-mode", "dontAsk") {
		t.Fatal("missing isolation options")
	}
	var settings struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	for i := range args {
		if args[i] == "--settings" {
			if json.Unmarshal([]byte(args[i+1]), &settings) != nil {
				t.Fatal("invalid fixed settings")
			}
		}
	}
	for _, tool := range []string{"Bash", "PowerShell", "Read", "Write", "Edit", "REPL", "Mods", "Agent", "ComputerUse", "CronCreate", "Skill"} {
		if !strings.Contains(" "+strings.Join(settings.Permissions.Deny, " ")+" ", " "+tool+" ") {
			t.Fatal("builtin escaped deny set", tool)
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--dangerously") || arg == "--resume" {
			t.Fatal("unexpected execution authority")
		}
	}
}
