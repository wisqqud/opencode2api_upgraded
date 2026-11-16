package gateway

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	wire "opencode2api/internal/protocol"
)

func TestShapeOfficialPrefixAndExactReuse(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, v1 := range []bool{false, true} {
			canonical := officialTool(protocol, "read", v1)
			data, err := json.Marshal(canonical)
			if err != nil {
				t.Fatal(err)
			}
			var client map[string]any
			if err := json.Unmarshal(data, &client); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"tools": []any{client, client}}
			if protocol == wire.Chat {
				payload["messages"] = []any{
					map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}},
					map[string]any{"role": "tool", "tool_call_id": "c1", "content": "done"},
				}
				payload["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "read"}}
			}
			renames, _ := renameAndCompleteTools(payload, protocol, v1)
			tools := payload["tools"].([]any)
			want := officialToolset(protocol, v1)
			if len(tools) != len(want) {
				t.Fatalf("protocol=%v v1=%v tools=%d want=%d", protocol, v1, len(tools), len(want))
			}
			for i := range want {
				if !sameToolDefinition(tools[i].(map[string]any), want[i].(map[string]any)) {
					t.Fatalf("official prefix differs at %d", i)
				}
			}
			if wireName, ok := renames.Wire("read"); !ok || wireName != "read" {
				t.Fatal("reused read lost identity mapping")
			}
			if renames.Count() != 0 || renames.Restore("read") != "read" {
				t.Fatal("reused read counted as renamed or failed round trip")
			}
			if protocol == wire.Chat {
				if len(payload["messages"].([]any)) != 2 || payload["tool_choice"] == nil {
					t.Fatal("reused tool history or choice removed")
				}
			}
		}
	}
}

func TestShapeDifferentDefinitionGetsAliasSuffix(t *testing.T) {
	for _, change := range []string{"description", "schema", "strict", "extra"} {
		t.Run(change, func(t *testing.T) {
			data, _ := json.Marshal(officialTool(wire.Chat, "read", false))
			var client map[string]any
			if err := json.Unmarshal(data, &client); err != nil {
				t.Fatal(err)
			}
			fn := client["function"].(map[string]any)
			switch change {
			case "description":
				fn["description"] = "Client reader"
			case "schema":
				fn["parameters"] = map[string]any{"type": "object"}
			case "strict":
				delete(fn, "strict")
			case "extra":
				fn["custom"] = true
			}
			payload := map[string]any{"tools": []any{client}}
			renames, _ := renameAndCompleteTools(payload, wire.Chat, false)
			tools := payload["tools"].([]any)
			n := len(officialToolNames(false))
			if len(tools) != n+1 {
				t.Fatal("noncanonical caller tool not retained")
			}
			for i, name := range officialToolNames(false) {
				got, _ := renamableToolName(wire.Chat, tools[i].(map[string]any))
				if got != name {
					t.Fatalf("prefix[%d]=%s want %s", i, got, name)
				}
			}
			got, _ := renamableToolName(wire.Chat, tools[n].(map[string]any))
			if !strings.HasPrefix(got, "clnt_") || len(got) != 29 || renames.Restore(got) != "read" {
				t.Fatalf("bad alias or round trip: %s", got)
			}
		})
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
			if _, ok := renameAndCompleteTools(payload, tc.protocol, false); !ok {
				t.Fatal("renameAndCompleteTools reported no shaping")
			}
			got := tc.get(tc.entry)
			want := officialToolDescription("edit", false)
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
			renames, ok := renameAndCompleteTools(payload, protocol, v1)
			if !ok {
				t.Fatal("renameAndCompleteTools reported no shaping")
			}
			tools := payload["tools"].([]any)
			function := tools[len(officialToolNames(v1))].(map[string]any)
			if protocol == wire.Chat {
				function = function["function"].(map[string]any)
			}
			if function["description"] != officialToolDescription("edit", v1) {
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
		shaped, renames := prepareAnonymousBody(body, wire.Chat, v1)
		var payload map[string]any
		if err := json.Unmarshal(shaped, &payload); err != nil {
			t.Fatal(err)
		}
		tools := payload["tools"].([]any)
		officialCount := len(officialToolNames(v1))
		client := tools[officialCount].(map[string]any)["function"].(map[string]any)
		definition, ok := officialToolDefinition("edit", v1)
		if !ok || client["description"] != hostToolDescription("edit", definition.Description, v1) {
			t.Fatal("serialized client edit description differs from the captured lane definition")
		}
		if client["strict"] != true || renames.Restore(client["name"].(string)) != "eDiT" {
			t.Fatal("client strict flag or tool-name round trip changed")
		}
		params := client["parameters"].(map[string]any)
		if len(params["properties"].(map[string]any)) != 0 {
			t.Fatal("client schema replaced with official parameters")
		}
		other := tools[officialCount+1].(map[string]any)["function"].(map[string]any)
		if other["description"] != "Keep this client wording" {
			t.Fatal("unrelated client description changed")
		}
		for _, raw := range tools[:officialCount] {
			tool := raw.(map[string]any)["function"].(map[string]any)
			if tool["name"] == "edit" && tool["description"] != client["description"] {
				t.Fatal("client and appended official edit descriptions differ")
			}
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
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false)
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
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false)
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
					"name": "exec_command",
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
	renames, ok := renameAndCompleteTools(payload, wire.Chat, false)
	if !ok {
		t.Fatal("renameAndCompleteTools reported no shaping")
	}
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"comm"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"pwd\",\"timeout\":5000}"}}]}}]}`,
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
	if !strings.Contains(text, `"name":"exec_command"`) {
		t.Fatalf("stream lacks exec_command: %s", text)
	}
	if !strings.Contains(text, `\"cmd\":\"pwd\"`) {
		t.Fatalf("stream lacks cmd: %s", text)
	}
	if strings.Contains(text, `"timeout"`) {
		t.Fatalf("stream kept the dropped timeout argument: %s", text)
	}
}
