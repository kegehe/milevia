package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

const supervisionTargetVerificationContract = "milevia-verification-v1"

type supervisionTargetCheckInput struct {
	RevisionID string              `json:"revisionId"`
	RulesHash  string              `json:"rulesHash"`
	Plan       supervisionTaskPlan `json:"plan"`
	Check      supervisionCheck    `json:"check"`
}

func (r *supervisionTargetRuntime) runVerification(ctx context.Context, execution targetexec.Execution, input targetexec.Input, handshake targetexec.Handshake) (targetexec.Receipt, error) {
	receipt := targetexec.Receipt{Quiet: true}
	var request supervisionTargetCheckInput
	if execution.Spec.Contract != supervisionTargetVerificationContract || input.Instructions != supervisionTargetVerificationContract || json.Unmarshal(input.Observation, &request) != nil {
		return receipt, targetexec.ErrConflict
	}
	admission, err := r.workspace(execution.Spec.WorkspaceID)
	if err != nil {
		return receipt, err
	}
	collected, err := collectSupervisionWorkspace(ctx, admission, r.storage)
	if err != nil {
		return receipt, err
	}
	if collected.RevisionID != request.RevisionID || collected.RulesHash != request.RulesHash {
		return receipt, errSupervisionSnapshotChanged
	}
	if err := validateSupervisionPlan(&request.Plan, collected.Manifest); err != nil {
		return receipt, err
	}
	if err := validateSupervisionCheck(request.Check, request.Plan, collected.Manifest); err != nil {
		return receipt, err
	}
	if request.Check.Directory == "" {
		request.Check.Directory = "."
	}
	cwd, err := canonicalWorkspacePath(filepath.Join(admission.OriginalPath, filepath.FromSlash(request.Check.Directory)))
	if err != nil || !isPathWithin(admission.CanonicalPath, cwd) {
		return receipt, errSupervisionSnapshotChanged
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(request.Check.TimeoutMillis)*time.Millisecond)
	defer cancel()
	sink := &supervisionTargetSink{runtime: r, execution: execution, cancel: cancel}
	result := supervisionCheckResult{Check: request.Check, ExitCode: -1, NotExecuted: true, StartedAt: time.Now().UTC()}
	binary, args, runErr := supervisionNativeVerificationCommand(request.Check.Program, request.Check.Args)
	var browserPath string
	var cleanup func()
	if request.Check.Browser != nil {
		if runErr = r.reserveArtifacts(execution); runErr == nil {
			binary, args, browserPath, cleanup, runErr = prepareSupervisionBrowserFiles(cwd, *request.Check.Browser, filepath.Join(r.storage, "supervision-target", "artifacts", execution.Spec.ExecutionID))
		}
	}
	if cleanup != nil {
		defer cleanup()
	}
	result.Executable = binary
	if runErr == nil {
		result.Environment, runErr = supervisionCheckEnvironment(ctx, admission.OriginalPath, request.Check)
	}
	stdout, stderr := &supervisionTargetOutput{sink: sink, stream: "stdout"}, &supervisionTargetOutput{sink: sink, stream: "stderr"}
	if runErr == nil {
		cmd := exec.Command(binary, args...)
		cmd.Dir = cwd
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		cmd.WaitDelay = agentProcessTeardownGrace
		cmd.Env = managedCLIEnvironment(nil, os.Environ(), "CI=1", "PYTHONDONTWRITEBYTECODE=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "AUTO_APPROVAL_TOKEN=", "AUTO_CONTROL_URL=")
		_, _, receipt.Quiet, runErr = runSupervisionContainedCommand(ctx, cmd, execution.Container, func(identity supervisionProcessIdentity, resume func() error) error {
			return handshake(targetexec.ProcessIdentity{Container: identity.Container, PID: identity.PID, Birth: identity.Birth, Executable: identity.Executable}, func() error {
				err := resume()
				if err == nil {
					result.NotExecuted = false
				}
				return err
			})
		})
	}
	if !receipt.Quiet {
		return receipt, runErr
	}
	// Finish temporary driver cleanup before measuring durable artifact bytes.
	if cleanup != nil {
		cleanup()
	}
	if request.Check.Browser != nil {
		facts, factsErr := r.store.Facts()
		if _, allocated := facts.Artifacts[execution.Spec.ExecutionID]; factsErr == nil && allocated {
			bytes, sizeErr := r.artifactBytes(execution.Spec.ExecutionID)
			if sizeErr == nil {
				sizeErr = r.store.SettleArtifacts(execution.Spec.SessionID, execution.Spec.ExecutionID, bytes)
			}
			runErr = errors.Join(runErr, sizeErr)
		} else {
			runErr = errors.Join(runErr, factsErr)
		}
	}
	if runErr == nil {
		result.ExitCode = 0
	} else {
		var exit *supervisionExitError
		if errors.As(runErr, &exit) {
			result.ExitCode = exit.Code
		}
	}
	result.Stdout, result.Stderr = redactAgentText(string(stdout.data)), redactAgentText(string(stderr.data))
	result.Truncated = stdout.truncated || stderr.truncated
	if request.Check.Browser != nil && browserPath != "" {
		data, reportErr := os.ReadFile(browserPath)
		image, imageErr := os.ReadFile(filepath.Join(filepath.Dir(browserPath), "screenshot.png"))
		var report struct {
			Passed bool `json:"passed"`
			Steps  []struct {
				Passed bool `json:"passed"`
			} `json:"steps"`
		}
		valid := reportErr == nil && len(data) < 256<<10 && json.Unmarshal(data, &report) == nil && report.Passed && len(report.Steps) == len(request.Check.Browser.Steps) && imageErr == nil && len(image) >= 8 && len(image) <= 20<<20 && bytes.Equal(image[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10})
		for _, step := range report.Steps {
			valid = valid && step.Passed
		}
		if reportErr == nil && len(data) < 256<<10 && json.Valid(data) {
			var body any
			if json.Unmarshal(data, &body) == nil {
				result.BrowserResult = mustJSON(redactSupervisionTargetValue(body))
			}
		}
		if !valid && runErr == nil {
			runErr = errors.New("目标浏览器报告或截图缺失、不完整或未通过")
		}
		if imageErr == nil {
			result.ScreenshotSHA256 = supervisionSHA256(image)
		}
	}
	if runErr == nil {
		environment, err := supervisionCheckEnvironment(ctx, admission.OriginalPath, request.Check)
		if err != nil {
			runErr = err
		} else if environment != result.Environment {
			runErr = errors.New("目标测试期间依赖环境变化")
		}
	}
	result.Interrupted = ctx.Err() != nil
	result.FinishedAt = time.Now().UTC()
	result.Error = redactAgentText(errorString(runErr))
	terminal, _ := supervisionTargetPayload(mustJSON(result))
	sink.append("verification.result", terminal)
	if sink.failure() == nil {
		receipt.ResultSHA256 = supervisionSHA256(terminal)
	}
	receipt.Usage = targetexec.Usage{Sequence: 1, ActiveMillis: time.Since(result.StartedAt).Milliseconds()}
	return receipt, errors.Join(runErr, sink.failure())
}

type supervisionTargetOutput struct {
	sink      *supervisionTargetSink
	stream    string
	data      []byte
	truncated bool
}

func (w *supervisionTargetOutput) Write(p []byte) (int, error) {
	n := len(p)
	available := (64 << 10) - len(w.data)
	if len(p) > available {
		w.truncated = true
		p = p[:available]
	}
	w.data = append(w.data, p...)
	if len(p) > 0 {
		w.sink.append("verification.output", mustJSON(map[string]string{"stream": w.stream, "text": redactAgentText(string(p))}))
	}
	return n, w.sink.failure()
}
