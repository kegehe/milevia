package app

// This journal is shared by every controller/helper running as the target user,
// independently of their databases and SSH connection names. A pipe closing is
// never evidence that an ordinary writer (or its descendants) has stopped.
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type workspaceGuardRecord struct {
	ID            string                      `json:"id"`
	Kind          string                      `json:"kind"` // operation or supervision
	Path          string                      `json:"path"`
	Identity      string                      `json:"identity,omitempty"`
	State         string                      `json:"state"`                // pending, owned, active, released (tombstone)
	ValidUntil    time.Time                   `json:"validUntil,omitempty"` // bounds only the initial ordinary begin, never the writer lifetime
	JournalID     string                      `json:"journalId,omitempty"`
	CLIContainer  string                      `json:"cliContainer,omitempty"` // immutable UUID; the target adds its native container prefix
	CLIStarted    bool                        `json:"cliStarted,omitempty"`
	CLILeaseUntil time.Time                   `json:"cliLeaseUntil,omitempty"`
	CLIProcess    *supervisionProcessIdentity `json:"cliProcess,omitempty"`
	helperSHA256  string                      // controller-only deployment hint, never a journal fact
}

type workspaceGuardFacts struct {
	Protocol int                             `json:"protocol"`
	ID       string                          `json:"id"`
	Records  map[string]workspaceGuardRecord `json:"records"`
}

// v2 adds authoritative ordinary-CLI containment facts. A v1 helper must refuse
// this journal rather than decode/rewrite it while discarding the new fields.
const workspaceGuardJournalProtocol = 2

func workspaceGuardDirectory(config Config) (string, error) {
	if config.WorkspaceGuardDir != "" { // Explicit embedding/test override, never inferred from DataDir.
		return filepath.Abs(config.WorkspaceGuardDir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "milevia", "workspace-protection"), nil
}

// The OS lock covers only read/compare/durable replacement, never a network
// request, process lifetime, SQLite transaction or model call.
func withWorkspaceGuard(ctx context.Context, config Config, fn func(*workspaceGuardFacts) error) error {
	dir, err := workspaceGuardDirectory(config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Refuse redirected state, including a redirected ancestor.
	for p := dir; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("共享目录保护存储被重定向或不可用")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	lockPath := filepath.Join(dir, "journal.lock")
	if info, err := os.Lstat(lockPath); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return targetexec.ErrConflict
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := lockWorkspaceGuard(file)
		if err != nil {
			return err
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer unlockWorkspaceGuard(file)
	name := filepath.Join(dir, "journal.json")
	facts := workspaceGuardFacts{Protocol: workspaceGuardJournalProtocol, ID: uuid.NewString(), Records: map[string]workspaceGuardRecord{}}
	if info, err := os.Lstat(name); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 32<<20 {
			return targetexec.ErrConflict
		}
		raw, err := os.ReadFile(name)
		// Missing persisted header fields are corruption, not a fresh journal.
		facts = workspaceGuardFacts{}
		if err != nil || json.Unmarshal(raw, &facts) != nil || (facts.Protocol != 1 && facts.Protocol != workspaceGuardJournalProtocol) || facts.ID == "" || facts.Records == nil {
			return errors.New("共享目录保护记录损坏，保留保护并等待核对")
		}
		facts.Protocol = workspaceGuardJournalProtocol
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := fn(&facts); err != nil {
		return err
	}
	raw, err := json.Marshal(facts)
	if err != nil || len(raw) > 32<<20 {
		return targetexec.ErrLimit
	}
	temporary, err := os.CreateTemp(dir, ".journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	_, writeErr := temporary.Write(raw)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return replaceWorkspaceGuard(temporary.Name(), name)
}

func workspaceGuardOverlap(a, b workspaceGuardRecord) bool {
	return a.Identity != "" && a.Identity == b.Identity || isPathWithin(a.Path, b.Path) || isPathWithin(b.Path, a.Path)
}

// A stable caller-generated ID makes lost replies queryable/retryable. Released
// IDs cannot resurrect when a delayed reserve/begin arrives after release.
func changeWorkspaceGuard(ctx context.Context, config Config, method string, requested workspaceGuardRecord) (workspaceGuardRecord, error) {
	var result workspaceGuardRecord
	if requested.ID == "" || len(requested.ID) > 128 || (requested.Kind != "operation" && requested.Kind != "supervision") {
		return result, targetexec.ErrProtocol
	}
	if method == "begin" || method == "reserve" {
		if method == "begin" && (requested.ValidUntil.IsZero() || !requested.ValidUntil.After(time.Now().UTC()) || requested.ValidUntil.After(time.Now().UTC().Add(2*time.Minute))) {
			return result, targetexec.ErrAuthority
		}
		if !filepath.IsAbs(requested.Path) {
			return result, targetexec.ErrConflict
		}
		canonical, err := canonicalWorkspacePath(requested.Path)
		if err != nil {
			return result, err
		}
		requested.Path = canonical
		if err := verifyWorkspaceGuardFilesystem(canonical); err != nil {
			return result, err
		}
		identity, err := workspaceDirectoryIdentity(canonical)
		if err != nil && (requested.Kind == "supervision" || !errors.Is(err, os.ErrNotExist)) {
			return result, err
		}
		if requested.Identity != "" && requested.Identity != identity {
			return result, targetexec.ErrConflict
		}
		requested.Identity = identity
		if requested.CLIContainer != "" {
			if requested.Kind != "operation" || !validSupervisionContainer(supervisionJobPrefix+requested.CLIContainer) || requested.CLIStarted || requested.CLIProcess != nil {
				return result, targetexec.ErrProtocol
			}
			if !requested.CLILeaseUntil.IsZero() && (!requested.CLILeaseUntil.After(time.Now().UTC()) || requested.CLILeaseUntil.After(time.Now().UTC().Add(30*time.Second))) {
				return result, targetexec.ErrAuthority
			}
			if err := supervisionLocalProcessCapability(); err != nil {
				return result, err
			}
		}
	}
	err := withWorkspaceGuard(ctx, config, func(facts *workspaceGuardFacts) error {
		if requested.JournalID != "" && requested.JournalID != facts.ID {
			return targetexec.ErrAuthority
		}
		settleScope := requested
		if prior, ok := facts.Records[requested.ID]; ok {
			settleScope = prior
		}
		for key, record := range facts.Records {
			if key != record.ID || (record.Kind != "operation" && record.Kind != "supervision") || (record.State != "pending" && record.State != "owned" && record.State != "active" && record.State != "released") || record.State != "released" && !filepath.IsAbs(record.Path) {
				return targetexec.ErrConflict
			}
			if record.Kind == "operation" && record.State == "released" && !record.ValidUntil.IsZero() && !record.ValidUntil.After(time.Now().UTC()) {
				delete(facts.Records, key)
				continue
			}
			if record.CLIContainer != "" {
				if record.Kind != "operation" || !validSupervisionContainer(supervisionJobPrefix+record.CLIContainer) || record.ValidUntil.IsZero() || record.CLIStarted && (record.CLIProcess == nil || record.CLIProcess.PID <= 0 || record.CLIProcess.Birth == "" || record.CLIProcess.Container != supervisionJobPrefix+record.CLIContainer || !filepath.IsAbs(record.CLIProcess.Directory)) || !record.CLIStarted && record.CLIProcess != nil {
					return targetexec.ErrConflict
				}
				// Only a kernel container fact can settle a launched CLI. Before
				// launch, keep its reservation until the immutable start window ends.
				if record.State != "released" && (record.ID == requested.ID || (method == "own" || method == "reserve") && workspaceGuardOverlap(settleScope, record)) && (record.CLIStarted || !record.ValidUntil.After(time.Now().UTC())) {
					quiet, err := supervisionContainerQuiescent(supervisionJobPrefix + record.CLIContainer)
					if err != nil {
						return err
					}
					if quiet {
						record.State = "released"
						facts.Records[key] = record
					}
				}
			}
			// Only ordinary released tokens with an expired begin window can be
			// compacted. Active/unknown writers never expire with their pipe.
			if record.Kind == "operation" && record.State == "released" && !record.ValidUntil.IsZero() && !record.ValidUntil.After(time.Now().UTC()) {
				delete(facts.Records, key)
			}
		}
		prior, exists := facts.Records[requested.ID]
		if exists && (prior.Kind != requested.Kind || requested.Path != "" && prior.Path != requested.Path || requested.Identity != "" && prior.Identity != requested.Identity || requested.CLIContainer != "" && prior.CLIContainer != requested.CLIContainer) {
			return targetexec.ErrConflict
		}
		switch method {
		case "stop-cli":
			if !exists || prior.Kind != "operation" || prior.State != "active" || prior.CLIContainer == "" {
				return targetexec.ErrAuthority
			}
			if err := stopSupervisionContainer(supervisionJobPrefix + prior.CLIContainer); err != nil {
				return err
			}
			prior.CLILeaseUntil = time.Now().UTC()
			facts.Records[prior.ID], result = prior, prior
		case "renew-cli":
			if !exists || prior.State != "active" || prior.CLIContainer == "" || prior.CLILeaseUntil.IsZero() || !prior.CLILeaseUntil.After(time.Now().UTC()) {
				return targetexec.ErrAuthority
			}
			prior.CLILeaseUntil = time.Now().UTC().Add(15 * time.Second)
			facts.Records[prior.ID], result = prior, prior
		case "begin", "reserve":
			if method == "begin" && requested.Kind != "operation" || method == "reserve" && requested.Kind != "supervision" {
				return targetexec.ErrProtocol
			}
			if exists {
				if prior.State == "released" {
					return targetexec.ErrAuthority
				}
				result = prior
				return nil
			}
			for _, other := range facts.Records {
				if other.State == "released" || !workspaceGuardOverlap(requested, other) {
					continue
				}
				if other.Kind == "supervision" {
					return errors.Join(errWorkspaceSupervised, targetexec.ErrNotQuiet)
				}
			}
			requested.State = "pending"
			if requested.Kind == "operation" {
				requested.State = "active"
			}
			facts.Records[requested.ID], result = requested, requested
		case "own", "query":
			if !exists {
				// With a pinned journal and an expired initial begin window, an
				// absent ordinary token cannot be recreated by a delayed begin.
				// Active writers are never compacted, even after this deadline.
				if method == "query" && requested.Kind == "operation" && requested.JournalID != "" && !requested.ValidUntil.IsZero() && !requested.ValidUntil.After(time.Now().UTC()) {
					result = requested
					result.State = "released"
					return nil
				}
				return targetexec.ErrConflict
			}
			if method == "own" {
				if prior.Kind != "supervision" || prior.State == "released" {
					return targetexec.ErrAuthority
				}
				for _, other := range facts.Records {
					if other.ID != prior.ID && other.State != "released" && workspaceGuardOverlap(prior, other) {
						return errors.Join(errWorkspaceNotQuiescent, targetexec.ErrNotQuiet)
					}
				}
				prior.State = "owned"
				facts.Records[prior.ID] = prior
			}
			result = prior
		case "release":
			if !exists { // Cancellation can overtake its begin/reserve.
				prior = requested
			}
			if prior.CLIContainer != "" {
				quiet, err := supervisionContainerQuiescent(supervisionJobPrefix + prior.CLIContainer)
				if err != nil {
					return err
				}
				if !quiet {
					return errWorkspaceNotQuiescent
				}
			}
			prior.State = "released"
			facts.Records[requested.ID], result = prior, prior
		default:
			return targetexec.ErrProtocol
		}
		return nil
	})
	return result, err
}

// Ordinary writers use a separate authenticated pipe. It takes no execution
// store ownership, so another controller can still check the common journal.
func ServeWorkspaceGuard(ctx context.Context, config Config, channel io.ReadWriteCloser) error {
	var id string
	if err := withWorkspaceGuard(ctx, config, func(f *workspaceGuardFacts) error { id = f.ID; return nil }); err != nil {
		return err
	}
	transport := &targetexec.Transport{EndpointIdentity: id, Extension: func(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
		if request.Method == "health" {
			return json.Marshal(map[string]any{"protocol": targetexec.ProtocolVersion, "workspaceGuard": true, "ordinaryCLIContainment": true})
		}
		if request.Method != "guard.begin" && request.Method != "guard.release" && request.Method != "guard.query" && request.Method != "guard.renew-cli" && request.Method != "guard.stop-cli" {
			return nil, targetexec.ErrProtocol
		}
		var record workspaceGuardRecord
		if json.Unmarshal(request.Parameters, &record) != nil || record.Kind != "operation" {
			return nil, targetexec.ErrProtocol
		}
		result, err := changeWorkspaceGuard(ctx, config, request.Method[len("guard."):], record)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}}
	return transport.Serve(ctx, channel)
}
