package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/models"
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
	got := opencodeSystemPrompt("opencode/test", "", true, cfg, environment, nil)
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
	if !injectOfficialPrompt(payload, wire.Chat, true, config.PromptConfig{}, nil, "") {
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

func assertV2ModelIdentity(t *testing.T, body []byte, protocol wire.Protocol, id, name string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	var system any
	switch protocol {
	case wire.Chat:
		system = payload["messages"].([]any)[0].(map[string]any)["content"]
	case wire.Anthropic:
		system = payload["system"]
	case wire.Responses:
		system = payload["instructions"]
	}
	prompt := strings.Join(textFragments(system), "\n")
	want := "# Your Model\n- Name: " + name + "\n- Provider ID: opencode\n- Model ID: " + id + "\n"
	if !strings.Contains(prompt, want) {
		t.Fatalf("%s system prompt lacks model identity %q", protocol, want)
	}
	if strings.Contains(prompt, "{{MODEL_") || strings.Count(prompt, "# Your Model\n") != 1 {
		t.Fatal("unresolved or duplicated model identity")
	}
	return prompt
}

func TestV2PromptUsesRequestedModelAcrossProtocols(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Anthropic, wire.Responses} {
		for _, tc := range []struct{ model, name, id, wantName string }{
			{"deepseek-v4.1-flash-free", "DeepSeek V4.1 Flash Free", "deepseek-v4.1-flash-free", "DeepSeek V4.1 Flash Free"},
			{"mimo-v2.6-flash-free", "MiMo-V2.6-Flash Free", "mimo-v2.6-flash-free", "MiMo-V2.6-Flash Free"},
			{"glm-5.3-flash-free", "", "glm-5.3-flash-free", "glm-5.3-flash-free"},
			{" opencode/custom-free ", " ", "custom-free", "custom-free"},
			{"", "", "unknown", "unknown"},
		} {
			t.Run(string(protocol)+"/"+tc.id, func(t *testing.T) {
				payload, err := wire.ConvertRequest(wire.Chat, protocol, map[string]any{
					"model": tc.model,
					"messages": []any{
						map[string]any{"role": "system", "content": "Keep the client's instructions."},
						map[string]any{"role": "user", "content": "Hello"},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				shaped, _ := prepareAnonymousBody(body, protocol, false, config.PromptConfig{}, nil, tc.name)
				prompt := assertV2ModelIdentity(t, shaped, protocol, tc.id, tc.wantName)
				if !strings.Contains(prompt, "Keep the client's instructions.") {
					t.Fatal("client instructions were lost")
				}
				if tc.id != "deepseek-v4.1-flash-free" && strings.Contains(prompt, "DeepSeek V4.1 Flash Free") {
					t.Fatal("another model received the DeepSeek identity")
				}
			})
		}
	}
}

func TestKeyAndProbePromptsUseCachedModelName(t *testing.T) {
	const model = "mimo-v2.6-flash-free"
	const name = "MiMo-V2.6-Flash Free"
	configPath := filepath.Join(t.TempDir(), "config.json")
	cache, err := json.Marshal(map[string]any{
		"updated_at": time.Now().UTC(),
		"models":     map[string]models.Price{model: {ID: model, Name: name}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath+".models.dev.json", cache, 0600); err != nil {
		t.Fatal(err)
	}
	catalog := models.Catalog(config.TierZen, nil)
	catalog.SetPricingStore(models.PricingStore(configPath, nil))
	g := &Gateway{catalog: catalog}
	g.cfg.Upstream.Zen = "https://opencode.ai/inference"
	g.cfg.Upstream.Go = "https://opencode.ai/zen/go"
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Anthropic, wire.Responses} {
		payload, err := wire.ConvertRequest(wire.Chat, protocol, map[string]any{
			"model": model, "messages": []any{map[string]any{"role": "user", "content": "Hello"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, tier := range []config.Tier{config.TierZen, config.TierGo} {
			shaped, _, changed := g.shapeKeyBody(body, models.Route{ID: model, Protocol: protocol}, tier)
			if !changed {
				t.Fatal("free model was not shaped")
			}
			assertV2ModelIdentity(t, shaped, protocol, model, name)
		}
		probe, err := probePayload(model, protocol, "", false, config.PromptConfig{}, nil, catalog.ModelName(model))
		if err != nil {
			t.Fatal(err)
		}
		assertV2ModelIdentity(t, probe, protocol, model, name)
	}
}

func TestV2PromptPreservesClientOfficialIdentity(t *testing.T) {
	client := opencodePromptMarkerV2 + ". Client-supplied model identity."
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Anthropic, wire.Responses} {
		payload, err := wire.ConvertRequest(wire.Chat, protocol, map[string]any{
			"model":    "mimo-v2.6-flash-free",
			"messages": []any{map[string]any{"role": "system", "content": client}, map[string]any{"role": "user", "content": "Hello"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		before, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if injectOfficialPrompt(payload, protocol, false, config.PromptConfig{}, nil, "MiMo-V2.6-Flash Free") {
			t.Fatal("client official prompt was injected again")
		}
		after, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("client official prompt changed")
		}
	}
}
