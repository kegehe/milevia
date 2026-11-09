package targetexec

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

const maxHelperBytes = 128 << 20

type HelperArtifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type HelperBundle struct {
	Protocol    int              `json:"protocol"`
	Fingerprint string           `json:"fingerprint"`
	GoVersion   string           `json:"goVersion"`
	LaunchArgs  []string         `json:"launchArgs"`
	Artifacts   []HelperArtifact `json:"artifacts"`
}

// The trusted installation bundle determines what can be deployed. Neither a
// role result, remote path nor target response can select arbitrary local code.
func LoadHelper(bundleDirectory, targetOS, arch string) (HelperArtifact, []byte, error) {
	root, err := os.OpenRoot(bundleDirectory)
	if err != nil {
		return HelperArtifact{}, nil, err
	}
	defer root.Close()
	f, err := root.Open("manifest.json")
	if err != nil {
		return HelperArtifact{}, nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	f.Close()
	var bundle HelperBundle
	if err != nil || len(raw) > 64<<10 || strictPacket(raw, &bundle) != nil || bundle.Protocol != ProtocolVersion || !validHash(bundle.Fingerprint) || len(bundle.LaunchArgs) != 2 || bundle.LaunchArgs[0] != "--mode" || bundle.LaunchArgs[1] != "supervision-target" || len(bundle.Artifacts) < 1 || len(bundle.Artifacts) > 4 {
		return HelperArtifact{}, nil, ErrProtocol
	}
	seen := map[string]bool{}
	var selected HelperArtifact
	for _, artifact := range bundle.Artifacts {
		key := artifact.OS + "-" + artifact.Arch
		file := "milevia-supervision-target-" + key
		if artifact.OS == "windows" {
			file += ".exe"
		} else if artifact.OS != "linux" {
			return HelperArtifact{}, nil, ErrProtocol
		}
		if artifact.Arch != "amd64" && artifact.Arch != "arm64" || seen[key] || artifact.File != file || !validHash(artifact.SHA256) || artifact.Size < 1 || artifact.Size > maxHelperBytes {
			return HelperArtifact{}, nil, ErrProtocol
		}
		seen[key] = true
		if artifact.OS == targetOS && artifact.Arch == arch {
			selected = artifact
		}
	}
	if selected.File == "" {
		return HelperArtifact{}, nil, errors.New("installation bundle has no helper for the target platform")
	}
	info, err := root.Lstat(selected.File)
	if err != nil || !info.Mode().IsRegular() || info.Size() != selected.Size {
		return HelperArtifact{}, nil, ErrConflict
	}
	f, err = root.Open(selected.File)
	if err != nil {
		return HelperArtifact{}, nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxHelperBytes+1))
	if err != nil || int64(len(body)) != selected.Size || hashBytes(body) != selected.SHA256 {
		return HelperArtifact{}, nil, ErrConflict
	}
	// A manifest cannot trick platform selection with executable extensions.
	if filepath.Base(selected.File) != selected.File {
		return HelperArtifact{}, nil, ErrConflict
	}
	return selected, body, nil
}
