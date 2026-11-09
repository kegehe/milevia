package app

import "testing"

func TestWorkspaceGuardWindowsDriveKeepsPhysicalOwnerAcrossRunnerSelections(t *testing.T) {
	s := &Server{}
	path := t.TempDir()
	native, canonical, err := s.workspaceGuardTargetPath(Project{Runner: "windows-local", Path: path})
	if err != nil || !native.local() {
		t.Fatal("native Windows directory did not resolve", native, err)
	}
	legacy, alias, err := s.workspaceGuardTargetPath(Project{Runner: "wsl-local", Path: path})
	if err != nil || legacy.Key != native.Key || alias != canonical {
		t.Fatal("ordinary Windows directory acquired a different owner through WSL selection", legacy, alias, err)
	}
	if s.supervisionProjectRoute(Project{Runner: "wsl-local", Path: path}).Environment != agentTargetEnvWSL {
		t.Fatal("ordinary directory protection changed the supervision execution route")
	}
}
