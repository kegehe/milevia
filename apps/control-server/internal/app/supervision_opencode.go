package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const supervisionOpenCodeContract = "opencode-controlled-input-v1"

func supervisionNativeOpenCodeBinary(binary string) (string, error) {
	abs, err := filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" && strings.EqualFold(filepath.Ext(abs), ".cmd") {
		raw, err := os.ReadFile(abs)
		if err != nil {
			return "", err
		}
		const header = "@ECHO off\nGOTO start\n:find_dp0\nSET dp0=%~dp0\nEXIT /b\n:start\nSETLOCAL\nCALL :find_dp0\n"
		normalize := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
		found := false
		for _, relative := range []string{`node_modules\opencode-ai\bin\opencode.exe`, `..\opencode-ai\bin\opencode.exe`} {
			if normalize(string(raw)) == normalize(header+`"%dp0%\`+relative+`" %*`) {
				abs, found = filepath.Join(filepath.Dir(abs), relative), true
				break
			}
		}
		if !found {
			return "", errors.New("托管 OpenCode 不解释自定义启动脚本")
		}
	}
	return supervisionNativeClaudeBinary(abs)
}

func (r *opencodeCLIRunner) supervisionCLIInfo(ctx context.Context, probe bool) (supervisionCLIInfo, error) {
	info := supervisionCLIInfo{AgentID: "opencode", Contract: supervisionOpenCodeContract}
	binary := r.opencodeBinary()
	if binary == "" {
		binary = "opencode"
	}
	binary, err := exec.LookPath(binary)
	if err != nil {
		return info, errors.Join(errSupervisionCLINotInstalled, errors.New("目标环境未安装 OpenCode"))
	}
	info.Path, err = supervisionNativeOpenCodeBinary(binary)
	if err != nil || !probe {
		return info, err
	}
	dir, err := os.MkdirTemp("", "milevia-supervision-opencode-probe-")
	if err != nil {
		return info, err
	}
	defer os.RemoveAll(dir)
	environment := supervisionOpenCodeEnvironment(dir, `{}`, `{}`, r.gitBashPath)
	for _, args := range [][]string{{"--version"}, {"run", "--help"}} {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cmd := exec.CommandContext(probeCtx, info.Path, args...)
		cmd.Dir, cmd.Env = dir, environment
		raw, diagnostic, probeErr := runProbeCommandStreams(probeCtx, cmd)
		cancel()
		if probeErr != nil {
			return info, errors.New("无法核对 OpenCode 托管协议")
		}
		if len(args) == 1 {
			info.Version = agentVersionFromOutput(string(raw))
			if info.Version != "1.19.5" {
				return info, errors.New("该 OpenCode 版本尚未通过托管权限验证（要求 1.19.5）")
			}
		} else {
			// yargs emits help on stderr in a redirected, noninteractive run.
			raw = append(raw, diagnostic...)
			for _, flag := range []string{"--pure", "--format", "--agent", "--model", "--variant"} {
				if !strings.Contains(string(raw), flag) {
					return info, fmt.Errorf("OpenCode 缺少托管隔离开关 %s", flag)
				}
			}
		}
	}
	return info, nil
}

func (r *opencodeCLIRunner) runSupervisionScheduler(ctx context.Context, request supervisionSchedulerCLIRequest, sink AgentRunSink) (receipt supervisionSchedulerCLIReceipt, resultErr error) {
	receipt = supervisionSchedulerCLIReceipt{AgentID: "opencode", Contract: supervisionOpenCodeContract, ContainerQuiet: true}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Minute {
		return receipt, errors.New("OpenCode 托管执行需要不超过30分钟的有效期限")
	}
	var input supervisionSchedulerInput
	if sink == nil || request.Handshake == nil || request.Container == "" || request.Profile != nil || len(request.Input) > maxSupervisionSchedulerInputBytes || json.Unmarshal(request.Input, &input) != nil || input.SchemaVersion != 1 || input.Decision.ID == "" || input.Decision.SessionID == "" || input.Decision.ExecutionID == "" {
		return receipt, errors.New("OpenCode 托管输入、认证来源或启动授权无效")
	}
	if request.ReasoningEffort != "" && !opencodeVariantPattern.MatchString(request.ReasoningEffort) {
		return receipt, errors.New("OpenCode 思考强度无效")
	}
	info, err := r.supervisionCLIInfo(ctx, true)
	if err != nil {
		return receipt, err
	}
	config, auth, model, err := supervisionOpenCodeConfiguration(request.Model, request.SystemPrompt)
	if err != nil {
		return receipt, err
	}
	info.EffectiveModel, info.EffectiveReasoningEffort = model, request.ReasoningEffort
	if request.Resolved != nil {
		if err := request.Resolved(info); err != nil {
			return receipt, err
		}
	}
	receipt.CLIPath, receipt.CLIVersion, receipt.InputSHA256 = info.Path, info.Version, supervisionSHA256(request.Input)
	dir, err := os.MkdirTemp("", "milevia-supervision-opencode-")
	if err != nil {
		return receipt, err
	}
	defer func() {
		if receipt.ContainerQuiet {
			_ = os.RemoveAll(dir)
		}
	}()
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		return receipt, err
	}
	auth, err = prepareSupervisionOpenCodeAuth(dir, auth, model)
	if err != nil {
		return receipt, err
	}
	args := []string{"run", "--pure", "--format", "json", "--agent", "milevia-supervision", "--model", model}
	if request.ReasoningEffort != "" {
		args = append(args, "--variant", request.ReasoningEffort)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := newSupervisionOpenCodeStream(sink, cancel)
	cmd := exec.Command(info.Path, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = work, supervisionOpenCodeEnvironment(dir, config, auth, r.gitBashPath), bytes.NewReader(request.Input)
	cmd.Stdout, cmd.Stderr = stream, &supervisionSchedulerStderr{}
	cmd.WaitDelay = agentProcessTeardownGrace
	configureProcessGroup(cmd)
	receipt.ProcessStarted, receipt.ProcessWaited, receipt.ContainerQuiet, err = runSupervisionContainedCommand(runCtx, cmd, request.Container, request.Handshake)
	if !receipt.ContainerQuiet {
		return receipt, errors.Join(err, stream.failure(), errors.New("OpenCode 容器或输出尚未确认收敛"))
	}
	stream.flush()
	receipt.Initialized, receipt.Output = stream.initialized, stream.output
	if failure := stream.failure(); failure != nil {
		return receipt, failure
	}
	if ctx.Err() != nil {
		return receipt, ctx.Err()
	}
	if err != nil {
		return receipt, fmt.Errorf("OpenCode 托管运行失败：%w", err)
	}
	if !stream.completed {
		return receipt, errors.New("OpenCode 托管回合缺少完整结果")
	}
	return receipt, nil
}

func supervisionOpenCodeEnvironment(home, config, auth, gitBash string) []string {
	base := make([]string, 0, len(os.Environ()))
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "OPENCODE_") || strings.HasPrefix(upper, "BUN_") || strings.HasPrefix(upper, "NODE_") || strings.HasPrefix(upper, "XDG_") {
			continue
		}
		base = append(base, item)
	}
	injected := []string{"OPENCODE_CONFIG_CONTENT=" + config, "OPENCODE_AUTH_CONTENT=" + auth, "OPENCODE_TEST_HOME=" + home, `OPENCODE_PERMISSION={"*":"deny"}`, "OPENCODE_PURE=1", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_DEFAULT_PLUGINS=1", "OPENCODE_DISABLE_EXTERNAL_SKILLS=1", "OPENCODE_DISABLE_CLAUDE_CODE=1", "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_LSP_DOWNLOAD=1", "AUTO_APPROVAL_TOKEN=", "AUTO_APPROVAL_RUN_ID=", "AUTO_CONTROL_URL="}
	for _, key := range []string{"CONFIG", "DATA", "CACHE", "STATE"} {
		injected = append(injected, "XDG_"+key+"_HOME="+filepath.Join(home, strings.ToLower(key)))
	}
	if gitBash != "" {
		injected = append(injected, "OPENCODE_GIT_BASH_PATH="+gitBash)
	}
	return managedCLIEnvironment(nil, base, injected...)
}
