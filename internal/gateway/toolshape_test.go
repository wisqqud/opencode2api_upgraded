package gateway

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

// TestShapeAppendsOfficialSetAfterClientTools pins the lane shape: the caller's
// tools are renamed into the mcp_ namespace and the full official set is
// appended after them, so every official name the gateway relies on is present
// exactly once.
func TestShapeAppendsOfficialSetAfterClientTools(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, v1 := range []bool{false, true} {
			canonical := officialTool(protocol, "read", v1, "linux")
			data, err := json.Marshal(canonical)
			if err != nil {
				t.Fatal(err)
			}
			var client map[string]any
			if err := json.Unmarshal(data, &client); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"tools": []any{client}}
			if protocol == wire.Chat {
				payload["messages"] = []any{
					map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}},
					map[string]any{"role": "tool", "tool_call_id": "c1", "content": "done"},
				}
				payload["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "read"}}
			}
			renames, ok := renameAndCompleteTools(payload, protocol, v1, "linux")
			if !ok {
				t.Fatal("renameAndCompleteTools reported no shaping")
			}
			tools := payload["tools"].([]any)
			want := officialToolset(protocol, v1, "linux")
			if len(tools) != len(want)+1 {
				t.Fatalf("protocol=%v v1=%v tools=%d want=%d+1", protocol, v1, len(tools), len(want))
			}
			for i := range want {
				if !reflect.DeepEqual(tools[i+1], want[i]) {
					t.Fatalf("official set differs at %d", i)
				}
			}
			wireName, ok := renames.Wire("read")
			if !ok || !strings.HasPrefix(wireName, wire.ClientToolPrefix) || renames.Restore(wireName) != "read" {
				t.Fatalf("client read wire name %q (ok=%v) does not round trip", wireName, ok)
			}
			if protocol == wire.Chat {
				if len(payload["messages"].([]any)) != 2 || payload["tool_choice"] == nil {
					t.Fatal("tool history or choice removed")
				}
			}
		}
	}
}

// TestShapeRenamesClientToolsToMCPPrefix covers the wire name itself: a client
// tool never reaches the free tier under its own name.
func TestShapeRenamesClientToolsToMCPPrefix(t *testing.T) {
	payload := map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "ReportFindings", "description": "Client tool", "parameters": map[string]any{"type": "object"},
	}}}}
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false, "")
	if !ok {
		t.Fatal("renameAndCompleteTools reported no shaping")
	}
	got, _ := renamableToolName(wire.Chat, payload["tools"].([]any)[0].(map[string]any))
	if !strings.HasPrefix(got, wire.ClientToolPrefix) || len(got) != len(wire.ClientToolPrefix)+24 {
		t.Fatalf("wire name = %q", got)
	}
	if renames.Restore(got) != "ReportFindings" {
		t.Fatalf("round trip of %q = %q", got, renames.Restore(got))
	}
}

func TestShapeNormalizesClientEditDescription(t *testing.T) {
	clientParameters := map[string]any{
		"type":       "object",
		"properties": map[string]any{"custom": map[string]any{"type": "string"}},
	}
	for _, tc := range []struct {
		name     string
		protocol wire.Protocol
		entry    map[string]any
		get      func(map[string]any) map[string]any
	}{
		{
			name:     "chat",
			protocol: wire.Chat,
			entry: map[string]any{"type": "function", "function": map[string]any{
				"name": "edit", "description": "client description", "parameters": clientParameters,
			}},
			get: func(entry map[string]any) map[string]any { return entry["function"].(map[string]any) },
		},
		{
			name:     "responses",
			protocol: wire.Responses,
			entry:    map[string]any{"type": "function", "name": "edit", "description": "client description", "parameters": clientParameters},
			get:      func(entry map[string]any) map[string]any { return entry },
		},
		{
			name:     "anthropic",
			protocol: wire.Anthropic,
			entry:    map[string]any{"name": "edit", "description": "client description", "input_schema": clientParameters},
			get:      func(entry map[string]any) map[string]any { return entry },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"tools": []any{tc.entry}}
			if _, ok := renameAndCompleteTools(payload, tc.protocol, false, ""); !ok {
				t.Fatal("renameAndCompleteTools reported no shaping")
			}
			got := tc.get(tc.entry)
			want := officialDescription("edit", false, "")
			if got["description"] != want {
				t.Fatalf("description = %q; want official %q", got["description"], want)
			}
			if !reflect.DeepEqual(got["parameters"], clientParameters) && tc.protocol != wire.Anthropic {
				t.Fatal("client parameters changed")
			}
			if tc.protocol == wire.Anthropic && !reflect.DeepEqual(got["input_schema"], clientParameters) {
				t.Fatal("client input schema changed")
			}
		})
	}
}

func TestShapeNormalizesLegacyEditDescription(t *testing.T) {
	for _, v1 := range []bool{false, true} {
		for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses} {
			payload := map[string]any{"functions": []any{map[string]any{
				"name": "Edit", "description": "client description", "parameters": map[string]any{"type": "object"},
			}}}
			renames, ok := renameAndCompleteTools(payload, protocol, v1, "")
			if !ok {
				t.Fatal("renameAndCompleteTools reported no shaping")
			}
			tools := payload["tools"].([]any)
			function := tools[0].(map[string]any)
			if protocol == wire.Chat {
				function = function["function"].(map[string]any)
			}
			if function["description"] != officialDescription("edit", v1, "") {
				t.Fatalf("legacy description = %q; want official for v1=%v", function["description"], v1)
			}
			if got := renames.Restore(function["name"].(string)); got != "Edit" {
				t.Fatalf("legacy tool restored to %q; want Edit", got)
			}
		}
	}
}

func TestPrepareAnonymousBodyKeepsOfficialEditDescription(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"OK"}],"tools":[{"type":"function","function":{"name":"eDiT","description":"Edit an existing UTF-8 text file by replacing literal text.","parameters":{"type":"object","properties":{}},"strict":true}},{"type":"function","function":{"name":"other","description":"Keep this client wording","parameters":{"type":"object","properties":{}}}}]}`)
	for _, v1 := range []bool{false, true} {
		shaped, renames := prepareAnonymousBody(body, wire.Chat, v1, config.PromptConfig{Disabled: true}, nil, "")
		var payload map[string]any
		if err := json.Unmarshal(shaped, &payload); err != nil {
			t.Fatal(err)
		}
		tools := payload["tools"].([]any)
		client := tools[0].(map[string]any)["function"].(map[string]any)
		if client["description"] != officialDescription("edit", v1, "") {
			t.Fatalf("serialized client edit description = %q", client["description"])
		}
		if client["strict"] != true || renames.Restore(client["name"].(string)) != "eDiT" {
			t.Fatal("client strict flag or tool-name round trip changed")
		}
		params := client["parameters"].(map[string]any)
		if len(params["properties"].(map[string]any)) != 0 {
			t.Fatal("client schema replaced with official parameters")
		}
		other := tools[1].(map[string]any)["function"].(map[string]any)
		if other["description"] != "Keep this client wording" {
			t.Fatal("unrelated client description changed")
		}
		officialCount := len(officialToolNames(v1))
		if len(tools) != officialCount+2 {
			t.Fatalf("tools = %d; want %d", len(tools), officialCount+2)
		}
	}
}

// TestShapeBindsExecCommandArguments covers the codex 0.167 case: the client
// declares exec_command, the gateway binds the official shell tool to it, and
// the argument names the official schema uses must be translated to the ones
// exec_command declares.
func TestShapeBindsExecCommandArguments(t *testing.T) {
	payload := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "exec_command",
					"description": "Run a command",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd":     map[string]any{"type": "string"},
							"workdir": map[string]any{"type": "string"},
						},
						"required": []any{"cmd"},
					},
				},
			},
		},
	}
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false, "")
	if !ok {
		t.Fatal("renameAndCompleteTools reported no shaping")
	}
	client, bound := renames.Official("shell")
	if !bound || client != "exec_command" {
		t.Fatalf("shell bound to %q (bound=%v); want exec_command", client, bound)
	}
	mapping, ok := renames.OfficialArgs("shell")
	if !ok {
		t.Fatal("shell argument mapping missing")
	}
	if mapping["command"] != "cmd" {
		t.Fatalf("command maps to %q; want cmd", mapping["command"])
	}
	if mapping["workdir"] != "workdir" {
		t.Fatalf("workdir maps to %q; want workdir", mapping["workdir"])
	}
	for _, dropped := range []string{"timeout", "background"} {
		value, known := mapping[dropped]
		if !known || value != "" {
			t.Fatalf("%s maps to %q (known=%v); want dropped", dropped, value, known)
		}
	}
}

// TestShapeLeavesUnboundClientToolsAlone checks the mapping is only recorded
// for a client tool the official name actually bound to.
func TestShapeLeavesUnboundClientToolsAlone(t *testing.T) {
	payload := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "read_file",
					"description": "Read a file",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{"path": map[string]any{"type": "string"}},
					},
				},
			},
		},
	}
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false, "")
	if !ok {
		t.Fatal("renameAndCompleteTools reported no shaping")
	}
	if _, bound := renames.Official("shell"); bound {
		t.Fatal("shell bound to a client tool that is not exec_command")
	}
	if _, ok := renames.OfficialArgs("shell"); ok {
		t.Fatal("shell argument mapping recorded without a binding")
	}
}

// TestShapedStreamRestoresExecCommandArguments runs the whole path: a shaped
// request declares exec_command, the upstream answers with the official shell
// tool, and the stream rewriter must hand the client exec_command with cmd.
func TestShapedStreamRestoresExecCommandArguments(t *testing.T) {
	payload := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "exec_command",
					"description": "Run a command",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd":     map[string]any{"type": "string"},
							"workdir": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false, "")
	if !ok {
		t.Fatal("renameAndCompleteTools reported no shaping")
	}
	if _, bound := renames.Official("shell"); !bound {
		t.Fatal("shell was not bound to exec_command")
	}
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"pwd\",\"timeout\":5000}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	out, err := io.ReadAll(wire.RenameStreamReader(strings.NewReader(strings.Join(frames, "\n\n")+"\n\n"), wire.Chat, renames))
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, `"shell"`) {
		t.Fatalf("stream still carries the official name: %s", text)
	}
	if !strings.Contains(text, `"exec_command"`) {
		t.Fatalf("stream lacks exec_command: %s", text)
	}
	if !strings.Contains(text, `\"cmd\":\"pwd\"`) {
		t.Fatalf("stream lacks cmd: %s", text)
	}
	if strings.Contains(text, "timeout") {
		t.Fatalf("stream kept the dropped timeout argument: %s", text)
	}
}
