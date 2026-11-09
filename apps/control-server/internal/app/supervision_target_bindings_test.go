package app

import (
	"context"
	"testing"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetBindingCannotSilentlyFollowRunnerIdentityChange(t *testing.T) {
	s, _, _, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	ctx := context.Background()
	link := &supervisionTargetLink{TargetID: "persistent-target", Artifact: targetexec.HelperArtifact{OS: "linux", Arch: "amd64", SHA256: "first-helper"}}
	if err := s.recordSupervisionTargetBinding(ctx, "ssh-target", link); err != nil {
		t.Fatal(err)
	}
	link.Artifact.SHA256 = "upgraded-helper"
	if err := s.recordSupervisionTargetBinding(ctx, "ssh-target", link); err != nil {
		t.Fatal("safe helper upgrade refused", err)
	}
	for _, kind := range []string{"target", "os", "arch"} {
		copy := *link
		switch kind {
		case "target":
			copy.TargetID = "reinstalled-target"
		case "os":
			copy.Artifact.OS = "windows"
		case "arch":
			copy.Artifact.Arch = "arm64"
		}
		if err := s.recordSupervisionTargetBinding(ctx, "ssh-target", &copy); err == nil {
			t.Fatal("mapping silently followed changed identity", kind)
		}
	}
	stored, err := s.supervisionTargetBinding(ctx, "ssh-target")
	if err != nil || stored.TargetID != "persistent-target" || stored.HelperSHA256 != "upgraded-helper" || stored.TargetOS != "linux" || stored.TargetArch != "amd64" {
		t.Fatal(stored, err)
	}
}
