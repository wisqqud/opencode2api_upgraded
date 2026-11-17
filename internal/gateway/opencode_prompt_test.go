package gateway

import (
	"strings"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

func TestDetectPromptEnvironmentFromClient(t *testing.T) {
	text := "Current working directory: C:\\Users\\denis\\project\nWorkspace root folder: C:\\Users\\denis\\project\nIs directory a git repo: yes"
	environment := detectPromptEnvironment(text)
	if environment.WorkDir != `C:\Users\denis\project` {
		t.Fatalf("work dir = %q", environment.WorkDir)
	}
	if environment.Workspace != `C:\Users\denis\project` {
		t.Fatalf("workspace = %q", environment.Workspace)
	}
	if environment.GitRepo != "yes" {
		t.Fatalf("git repo = %q", environment.GitRepo)
	}
}

func TestOpencodeSystemPromptOverrides(t *testing.T) {
	cfg := config.PromptConfig{
		CustomBaseV1: true, BaseV1: "BASE",
		CustomEnvironment: true, Environment: "cwd={{cwd}};model={{model}};git={{git}};platform={{platform}}",
		CustomSkills: true, Skills: "SKILLS",
		Extra: "EXTRA",
	}
	environment := promptEnvironment{WorkDir: `C:\p`, Workspace: `C:\p`, GitRepo: "yes", Platform: "win32"}
	got := opencodeSystemPrompt("opencode/test", true, cfg, environment, nil)
	want := "BASEcwd=C:\\p;model=test;git=yes;platform=win32\nSKILLS\n\nEXTRA"
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

func TestDefaultPromptPartsAreWindows(t *testing.T) {
	defaults := DefaultPromptParts()
	if strings.Contains(defaults.BaseV1, "non-trivial bash command") {
		t.Fatal("v1 prompt still tells the model it has bash")
	}
	if !strings.Contains(defaults.BaseV1, "non-trivial PowerShell command") {
		t.Fatal("v1 prompt is missing the PowerShell wording")
	}
	if !strings.Contains(defaults.Environment, "Platform: {{platform}}") {
		t.Fatal("environment template lost its platform placeholder")
	}
}

func TestInjectOfficialPromptUsesClientProject(t *testing.T) {
	payload := map[string]any{
		"model":    "opencode/test",
		"messages": []any{map[string]any{"role": "system", "content": "Current working directory: C:\\work\\demo"}},
	}
	if !injectOfficialPrompt(payload, wire.Chat, true, config.PromptConfig{}, nil) {
		t.Fatal("prompt was not injected")
	}
	messages := payload["messages"].([]any)
	system := messages[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, `Working directory: C:\work\demo`) {
		t.Fatalf("injected prompt did not use the client project: %q", system)
	}
	if strings.Contains(system, "Platform: linux") {
		t.Fatal("injected prompt still reports linux")
	}
}
