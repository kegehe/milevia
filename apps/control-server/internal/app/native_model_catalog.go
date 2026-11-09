package app

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

var codebuddyModelChoices = regexp.MustCompile(`(?s)--model <model>\s+[^\r\n]*?Currently supported:\s*\(([^)]+)\)`)

// Do not infer account-specific models from prose/errors. OpenCode's plain
// output is one provider/model per line; Qoder versions may return structured
// model records. Unknown output falls back to manual input.
func parseNativeModelCatalog(agentID, output string) []AgentModelOption {
	output = cleanAuthOutput(output)
	var candidates []AgentModelOption
	if agentID == "codebuddy" {
		if match := codebuddyModelChoices.FindStringSubmatch(output); len(match) == 2 && len(match[1]) <= 8192 {
			for _, id := range strings.Split(match[1], ",") {
				candidates = append(candidates, AgentModelOption{ID: strings.TrimSpace(id)})
			}
		}
	}
	var records []AgentModelOption
	if json.Unmarshal([]byte(output), &records) == nil {
		candidates = records
	} else {
		var envelope struct {
			Models []AgentModelOption `json:"models"`
		}
		if json.Unmarshal([]byte(output), &envelope) == nil {
			candidates = envelope.Models
		}
	}
	if len(candidates) == 0 && agentID == "opencode" {
		for _, line := range strings.Split(output, "\n") {
			id := strings.TrimSpace(line)
			provider, model, found := strings.Cut(id, "/")
			// A URL or diagnostic path is not a provider/model record.
			if found && provider != "" && provider != "." && provider != ".." && model != "" && !strings.ContainsAny(provider, ":\\") && !strings.HasPrefix(model, "/") && modelOverridePattern.MatchString(id) {
				candidates = append(candidates, AgentModelOption{ID: id})
			}
		}
	}
	if len(candidates) == 0 && agentID == "cursor" {
		inModels := false
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if line == "Available models" {
				inModels = true
				continue
			}
			if !inModels || line == "" {
				continue
			}
			if strings.HasPrefix(line, "Tip:") {
				break
			}
			id, label, separated := strings.Cut(line, " - ")
			if !separated || strings.TrimSpace(label) == "" {
				continue
			}
			if modelOverridePattern.MatchString(id) {
				candidates = append(candidates, AgentModelOption{ID: id, Label: label})
			}
		}
	}
	result := []AgentModelOption{}
	seen := map[string]bool{}
	for _, model := range candidates {
		if len(model.ID) > maxModelOverrideLength || !modelOverridePattern.MatchString(model.ID) || seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		if model.Label == "" {
			model.Label = model.ID
		}
		result = append(result, model)
		if len(result) >= 512 {
			break
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (s *Server) nativeCatalogRunner(agentID, runnerID, projectPath string) AgentRunner {
	if strings.HasPrefix(runnerID, "ssh-") {
		if s.runnerRegistry != nil {
			runner, _ := s.runnerRegistry.get(runnerID)
			return runner
		}
		return nil
	}
	target := s.resolveAgentTargetEnv(runnerID, projectPath)
	if agentID == "opencode" {
		return s.opencodeRunnerFor(target)
	}
	if agentID == "cursor" && isLocalRunnerID(runnerID) && ((runtime.GOOS == "windows") == (target == agentTargetEnvWindows)) {
		return s.cursorRunner
	}
	if agentID == "codebuddy" && isLocalRunnerID(runnerID) && ((runtime.GOOS == "windows") == (target == agentTargetEnvWindows)) {
		return s.codebuddyRunner
	}
	if agentID == "qoder" && isLocalRunnerID(runnerID) && ((runtime.GOOS == "windows") == (target == agentTargetEnvWindows)) {
		return s.qoderRunner
	}
	return nil
}

func (s *Server) nativeModelCatalog(ctx context.Context, agentID, runnerID, projectPath string) []AgentModelOption {
	ctx, cancel := context.WithTimeout(ctx, modelCatalogProbeTimeout)
	defer cancel()
	args := []string{"models"}
	if agentID == "qoder" {
		args = []string{"--list-models"}
	}
	if agentID == "codebuddy" {
		args = []string{"--help"}
	}
	var output string
	runner := s.nativeCatalogRunner(agentID, runnerID, projectPath)
	if runner == nil {
		return nil
	}
	if _, cross := runner.(crossShellRunner); !cross {
		var cmd *exec.Cmd
		if agentID == "opencode" {
			if runner, ok := runner.(*opencodeCLIRunner); ok && runner.opencodeBinary() != "" {
				cmd = opencodeCommandContext(ctx, runner.opencodeBinary(), args...)
			}
		} else if agentID == "qoder" {
			if runner, ok := runner.(*qoderCLIRunner); ok && runner.qoderBinary() != "" {
				cmd = qoderCommand(ctx, runner.qoderBinary(), args...)
			}
		} else if agentID == "cursor" {
			if runner, ok := runner.(*cursorCLIRunner); ok && runner.cursorBinary() != "" {
				cmd = cursorCommandContext(ctx, runner.cursorBinary(), args...)
			}
		} else if agentID == "codebuddy" {
			if runner, ok := runner.(*codebuddyCLIRunner); ok && runner.codebuddyBinary() != "" {
				cmd = codebuddyCommand(ctx, runner.codebuddyBinary(), args...)
			}
		}
		if cmd == nil {
			return nil
		}
		cmd.Dir = projectPath
		data, _, err := runProbeCommandStreamsLimit(ctx, cmd, 1<<20)
		if err != nil || len(data) > 1<<20 {
			return nil
		}
		output = string(data)
	} else {
		// Same registered target used for execution, never the server's local CLI.
		shell, ok := runner.(boundedCrossShellRunner)
		if !ok {
			return nil
		}
		entry, ok := agentByID(agentID)
		if !ok {
			return nil
		}
		if wsl, ok := runner.(*wslAgentRunner); ok {
			if converted, valid := uncToWslPath(projectPath, wsl.distro); valid {
				projectPath = converted
			} else if !strings.HasPrefix(projectPath, "/") {
				return nil
			}
		}
		for _, name := range agentCommandNames(entry) {
			quotedArgs := make([]string, 0, len(args))
			for _, arg := range args {
				quotedArgs = append(quotedArgs, shellQuote(arg))
			}
			command := shellQuote(name) + " " + strings.Join(quotedArgs, " ")
			if _, ok := runner.(*wslAgentRunner); ok {
				command = wslNativeProbeCommand(name, args...)
			}
			out, err := shell.crossRunLimited(ctx, "cd "+shellQuote(projectPath)+" && { "+command+"; }", 1<<20)
			if err == nil && len(out) <= 1<<20 {
				output = out
				break
			}
		}
	}
	return parseNativeModelCatalog(agentID, output)
}
