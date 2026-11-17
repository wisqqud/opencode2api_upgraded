package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	wire "opencode2api/internal/protocol"
)

// The fixture contains the changed definitions from a local capture of the
// official 2.3.0 CLI, independent of the assets embedded in production.
func TestV2ChangedToolsMatchOpenCode230(t *testing.T) {
	data, err := os.ReadFile("testdata/opencode_230_tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured map[string]opencodeToolDefinition
	if err := json.Unmarshal(data, &captured); err != nil {
		t.Fatal(err)
	}
	for name, want := range captured {
		t.Run(name, func(t *testing.T) {
			got := officialTool(wire.Chat, name, false, "linux")["function"].(map[string]any)
			if got["description"] != want.Description || !reflect.DeepEqual(got["parameters"], want.Parameters) {
				t.Errorf("%s differs from the official 2.3.0 description or schema", name)
			}
			if got["strict"] != false {
				t.Errorf("%s must retain the official strict:false flag", name)
			}
		})
	}
}

func TestWorktreeRootMatchesV2Workspace(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	nested := filepath.Join(workspace, "src")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if root, inGit := worktreeRoot(nested); root != nested || inGit {
		t.Fatalf("nongit workspace = (%q, %v); want (%q, false)", root, inGit, nested)
	}
	for _, linked := range []bool{false, true} {
		git := filepath.Join(workspace, ".git")
		if linked {
			if err := os.WriteFile(git, []byte("gitdir: /external/worktrees/example"), 0600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Mkdir(git, 0700); err != nil {
			t.Fatal(err)
		}
		if root, inGit := worktreeRoot(nested); root != workspace || !inGit {
			t.Fatalf("git workspace (linked=%v) = (%q, %v); want (%q, true)", linked, root, inGit, workspace)
		}
		if err := os.Remove(git); err != nil {
			t.Fatal(err)
		}
	}
}

func TestV2ShellDescriptionMatchesPlatform(t *testing.T) {
	loadOfficialTools()
	for _, tc := range []struct{ platform, want string }{
		{"linux", "on Linux using bash"},
		{"darwin", "on macOS using zsh"},
		{"win32", "on Windows using powershell"},
	} {
		got := officialDescription("shell", false, tc.platform)
		if !strings.Contains(got, tc.want) {
			t.Fatalf("%s shell description lacks %q: %q", tc.platform, tc.want, got)
		}
	}
}

func TestEnvironmentWorkspaceRootsByGeneration(t *testing.T) {
	opencodeEnvOnce.Do(resolveHostPaths)
	oldDirectory, oldRoot, oldInGit := opencodeWorkDir, opencodeRootDir, opencodeInGit
	t.Cleanup(func() { opencodeWorkDir, opencodeRootDir, opencodeInGit = oldDirectory, oldRoot, oldInGit })
	directory := t.TempDir()
	opencodeWorkDir, opencodeRootDir, opencodeInGit = directory, directory, false
	if got := opencodeEnvironmentV1("deepseek-v4.1-flash-free"); !strings.Contains(got, "Workspace root folder: "+workspaceRoot(directory)+"\n") {
		t.Fatalf("v1 nongit environment no longer reports the filesystem root: %q", got)
	}
	if got := opencodeEnvironmentV2(); !strings.Contains(got, "Workspace root folder: "+directory+"\n") {
		t.Fatalf("v2 nongit environment does not report the current workspace: %q", got)
	}
}
