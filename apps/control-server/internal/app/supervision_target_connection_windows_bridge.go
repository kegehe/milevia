package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// The reverse bridge uses the existing Windows PowerShell interop entry point.
// Scripts are trusted code; the only embedded data is base64-encoded JSON.
// Uploaded executables and protocol bytes never pass through a text pipeline.
const supervisionWindowsTargetDirectories = `
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
function Ensure-TargetDirectory([string]$directory) {
 $full = [IO.Path]::GetFullPath($directory)
 if ($full -notmatch '^[A-Za-z]:\\') { throw 'Target storage requires a Windows drive path' }
 $ancestors = New-Object 'System.Collections.Generic.List[string]'
 $current = $full
 while ($current) {
  $ancestors.Add($current)
  $parent = [IO.Directory]::GetParent($current)
  if ($null -eq $parent) { break }
  $current = $parent.FullName
 }
 for ($i = $ancestors.Count - 1; $i -ge 0; $i--) {
  $item = $ancestors[$i]
  if ([IO.File]::Exists($item)) { throw 'Target storage component is a file' }
  if ([IO.Directory]::Exists($item)) {
   if (([IO.File]::GetAttributes($item) -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Target storage cannot traverse a reparse point' }
  } else { [IO.Directory]::CreateDirectory($item) | Out-Null }
 }
 return $full
}
function Confirm-TargetFile([string]$file, [string]$digest) {
 if (![IO.File]::Exists($file) -or (([IO.File]::GetAttributes($file) -band [IO.FileAttributes]::ReparsePoint) -ne 0)) { throw 'Target executable is missing or redirected' }
 if ((Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant() -cne $digest) { throw 'Target executable digest mismatch' }
}
`

const supervisionWindowsTargetInstall = `
try {
 $targetHome = $request.home
 if (!$targetHome) { $targetHome = [Environment]::GetFolderPath('UserProfile') }
 $targetHome = Ensure-TargetDirectory $targetHome
 $base = Ensure-TargetDirectory ([IO.Path]::Combine($targetHome, '.local', 'share', 'milevia', 'supervision-targets', $request.digest))
 $file = [IO.Path]::Combine($base, 'helper.exe')
 $source = [Console]::OpenStandardInput()
 if ([IO.File]::Exists($file)) {
  Confirm-TargetFile $file $request.digest
  $source.CopyTo([IO.Stream]::Null)
 } else {
  $temporary = [IO.Path]::Combine($base, ('upload-' + [Guid]::NewGuid().ToString('N') + '.tmp'))
  try {
   $output = New-Object IO.FileStream($temporary, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
   try { $source.CopyTo($output); $output.Flush($true) } finally { $output.Dispose() }
   Confirm-TargetFile $temporary $request.digest
   try { [IO.File]::Move($temporary, $file) } catch {
    if (![IO.File]::Exists($file)) { throw }
    Confirm-TargetFile $file $request.digest
   }
  } finally { if ([IO.File]::Exists($temporary)) { Remove-Item -LiteralPath $temporary -Force } }
 }
 Confirm-TargetFile $file $request.digest
 [Console]::Out.WriteLine('installed')
} catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }
`

const supervisionWindowsTargetLaunch = `
try {
 Add-Type -TypeDefinition @'
using System.IO;
using System.Threading.Tasks;
public static class MileviaTargetPipe {
 public static async Task Pump(Stream source, Stream destination) {
  byte[] buffer = new byte[16384];
  int count;
  while ((count = await source.ReadAsync(buffer, 0, buffer.Length).ConfigureAwait(false)) != 0) {
   await destination.WriteAsync(buffer, 0, count).ConfigureAwait(false);
   await destination.FlushAsync().ConfigureAwait(false);
  }
 }
}
'@
 $targetHome = $request.home
 if (!$targetHome) { $targetHome = [Environment]::GetFolderPath('UserProfile') }
 $targetHome = Ensure-TargetDirectory $targetHome
 $base = Ensure-TargetDirectory ([IO.Path]::Combine($targetHome, '.local', 'share', 'milevia', 'supervision-targets', $request.digest))
 $state = Ensure-TargetDirectory ([IO.Path]::Combine($targetHome, '.local', 'state', 'milevia', 'supervision-execution'))
 $file = [IO.Path]::Combine($base, 'helper.exe')
 Confirm-TargetFile $file $request.digest
 $start = New-Object Diagnostics.ProcessStartInfo
 $start.FileName = $file
 $mode = $request.mode
 if (!$mode) { $mode = 'supervision-target' }
 if ($mode -cne 'supervision-target' -and $mode -cne 'workspace-guard') { throw 'Invalid target mode' }
 $start.Arguments = '--mode ' + $mode + ' --data-dir "' + $state + '"'
 $start.UseShellExecute = $false
 $start.CreateNoWindow = $true
 $start.RedirectStandardInput = $true
 $start.RedirectStandardOutput = $true
 $start.RedirectStandardError = $true
 foreach ($name in @('AUTO_APPROVAL_TOKEN','AUTO_APPROVAL_RUN_ID','AUTO_CONTROL_URL','AUTO_DATABASE_PATH','AUTO_CLAUDE_PATH','AUTO_CODEX_PATH','AUTO_OPENCODE_PATH','AUTO_REMOTE_AGENT_TOKEN')) {
  $start.EnvironmentVariables.Remove($name)
 }
 $process = New-Object Diagnostics.Process
 $process.StartInfo = $start
 if (!$process.Start()) { throw 'Target executable failed to start' }
 $inputCopy = [MileviaTargetPipe]::Pump([Console]::OpenStandardInput(), $process.StandardInput.BaseStream)
 $outputCopy = [MileviaTargetPipe]::Pump($process.StandardOutput.BaseStream, [Console]::OpenStandardOutput())
 $errorCopy = [MileviaTargetPipe]::Pump($process.StandardError.BaseStream, [Console]::OpenStandardError())
 $inputClosed = $false
 while (!$process.HasExited) {
  if (!$inputClosed -and $inputCopy.IsCompleted) {
   $process.StandardInput.Close()
   $inputClosed = $true
  }
  Start-Sleep -Milliseconds 50
 }
 $process.WaitForExit()
 [Threading.Tasks.Task]::WaitAll([Threading.Tasks.Task[]]@($outputCopy, $errorCopy))
 exit $process.ExitCode
} catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }
`

func supervisionWindowsTargetScript(script, digest, home string, modes ...string) (string, error) {
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || len(home) > 4096 || strings.ContainsAny(home, "\x00\r\n\"") {
		return "", targetexec.ErrConflict
	}
	mode := "supervision-target"
	if len(modes) == 1 {
		mode = modes[0]
	}
	if len(modes) > 1 || mode != "supervision-target" && mode != "workspace-guard" {
		return "", targetexec.ErrProtocol
	}
	data := base64.StdEncoding.EncodeToString(mustJSON(map[string]string{"digest": digest, "home": home, "mode": mode}))
	return "$request = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + data + "')) | ConvertFrom-Json\n" + supervisionWindowsTargetDirectories + script, nil
}

func openSupervisionWindowsTarget(ctx context.Context, config Config, expectedTargetID, targetHome string) (*supervisionTargetLink, error) {
	return openSupervisionWindowsTargetMode(ctx, config, expectedTargetID, targetHome, "supervision-target")
}

func openSupervisionWindowsTargetMode(ctx context.Context, config Config, expectedTargetID, targetHome, mode string) (*supervisionTargetLink, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	platform, err := windowsBridgeProbe(probeCtx, `[Console]::Out.WriteLine('windows-' + $(if ([Environment]::Is64BitProcess -and $env:PROCESSOR_ARCHITECTURE -eq 'AMD64') { 'amd64' } else { 'unsupported' }))`)
	if err != nil || strings.TrimSpace(platform) != "windows-amd64" {
		return nil, errors.New("无法核对 Windows 目标平台与 amd64 进程运行时")
	}
	powershell, err := windowsSystemExecutable("powershell.exe")
	if err != nil {
		return nil, err
	}
	artifact, body, err := targetexec.LoadHelper(supervisionHelperDirectory(config), "windows", "amd64")
	if err != nil {
		return nil, fmt.Errorf("Windows 目标辅助程序不可用：%w", err)
	}
	install, err := supervisionWindowsTargetScript(supervisionWindowsTargetInstall, artifact.SHA256, targetHome)
	if err != nil {
		return nil, err
	}
	cmd := newWindowsPowerShellCommand(probeCtx, powershell, base64.StdEncoding.EncodeToString(utf16LE(install)))
	cmd.Stdin = bytes.NewReader(body)
	if _, err := runProbeCommand(probeCtx, cmd); err != nil {
		return nil, fmt.Errorf("Windows 目标辅助程序部署失败：%w", err)
	}
	launch, err := supervisionWindowsTargetScript(supervisionWindowsTargetLaunch, artifact.SHA256, targetHome, mode)
	if err != nil {
		return nil, err
	}
	// The runtime owns this process; the bounded probe context must not own it.
	command := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-EncodedCommand", base64.StdEncoding.EncodeToString(utf16LE(launch)))
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	stderr := &supervisionSchedulerStderr{}
	command.Stderr = stderr
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	pipe := &supervisionTargetPipe{reader: stdout, writer: stdin, stop: command.Process.Kill, wait: command.Wait, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			pipe.Close()
		case <-pipe.done:
		}
	}()
	client := targetexec.NewClient(pipe)
	hello, err := client.Call(probeCtx, targetexec.Request{ID: "connect", Method: "hello"})
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("Windows 目标握手失败：%w；%s", err, stderr.text())
	}
	if expectedTargetID != "" && hello.TargetID != expectedTargetID {
		client.Close()
		return nil, errors.New("Windows 目标持久身份已改变，原映射不能自动替换")
	}
	health, err := client.Call(probeCtx, targetexec.Request{ID: "health", Method: "health"})
	var status struct {
		Protocol           int    `json:"protocol"`
		OS                 string `json:"os"`
		Arch               string `json:"arch"`
		ProcessContainment bool   `json:"processContainment"`
		Guard              bool   `json:"workspaceGuard"`
	}
	if err != nil || json.Unmarshal(health.Data, &status) != nil || status.Protocol != targetexec.ProtocolVersion || (mode == "supervision-target" && (status.OS != artifact.OS || status.Arch != artifact.Arch || !status.ProcessContainment)) || (mode == "workspace-guard" && !status.Guard) {
		client.Close()
		return nil, errors.Join(errors.New("Windows 目标监督能力或实际平台尚未就绪"), err)
	}
	return &supervisionTargetLink{Client: client, TargetID: hello.TargetID, Artifact: artifact}, nil
}
