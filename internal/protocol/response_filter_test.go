package protocol

import (
	"encoding/json"
	"reflect"
	"testing"

	"opencode2api/internal/jsonutil"
)

func filteredResponseRenames() *ToolRenames {
	r := NewToolRenames()
	r.Add("ReadFile", "clnt_known")
	r.BindOfficial("read", "ReadFile")
	r.BindOfficial("orphan", "Undeclared")
	r.EnableResponseFilter()
	return r
}

func decodeFilteredResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	return payload
}

func TestResponseFilterIsExplicitAndRequiresRegisteredClient(t *testing.T) {
	var nilRenames *ToolRenames
	for _, r := range []*ToolRenames{nilRenames, NewToolRenames(), {}} {
		if r.RestrictsResponses() || !r.AllowsResponseTool("invented") {
			t.Fatal("rename-only context unexpectedly restricts responses")
		}
	}
	nilRenames.EnableResponseFilter()
	for _, r := range []*ToolRenames{NewToolRenames(), {}} {
		r.EnableResponseFilter()
		if r.Empty() || !r.RestrictsResponses() || r.AllowsResponseTool("invented") {
			t.Fatal("empty client tool list must remain an active filter")
		}
	}
	r := filteredResponseRenames()
	for _, name := range []string{"ReadFile", "clnt_known", "clint_known", "clit_known", "read", "readfile", "read-file"} {
		if !r.AllowsResponseTool(name) {
			t.Errorf("registered name %q was filtered", name)
		}
	}
	for _, name := range []string{"", "clnt_unknown", "clint_unknown", "orphan", "Undeclared", "shell", "invented"} {
		if r.AllowsResponseTool(name) {
			t.Errorf("unregistered name %q was allowed", name)
		}
	}
}

func TestResponseFilterChatMixedAndLegacy(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"kept","tool_calls":[{"id":"allow","type":"function","function":{"name":"clnt_known","arguments":"{}"}},{"id":"unknown","type":"function","function":{"name":"clnt_unknown"}},{"id":"official","type":"function","function":{"name":"shell"}},{"id":"invented","type":"function","function":{"name":"invented"}}],"function_call":{"name":"read","arguments":"{}"}},"finish_reason":"tool_calls"}],"usage":{"total_tokens":7}}`)
	out := decodeFilteredResponse(t, RenameResponseTool(body, Chat, filteredResponseRenames()))
	choice := jsonutil.SliceAt(out, "choices")[0].(map[string]any)
	message := jsonutil.MapAt(choice, "message")
	calls := jsonutil.SliceAt(message, "tool_calls")
	if len(calls) != 1 || jsonutil.StringAt(calls[0].(map[string]any), "function", "name") != "ReadFile" {
		t.Fatalf("mixed calls = %v", calls)
	}
	if jsonutil.StringAt(message, "function_call", "name") != "ReadFile" || choice["finish_reason"] != "tool_calls" || message["content"] != "kept" || jsonutil.IntAt(out, "usage", "total_tokens") != 7 {
		t.Fatalf("ordinary/legacy response changed unexpectedly: %v", out)
	}
}

func TestResponseFilterResponsesMixedOutputAndEnvelope(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		payload := map[string]any{
			"output": []any{
				map[string]any{"type": "message", "content": "kept"},
				map[string]any{"type": "function_call", "name": "clnt_known", "arguments": "{}"},
				map[string]any{"type": "custom_tool_call", "name": "read", "input": "raw"},
				map[string]any{"type": "tool_call", "name": "ReadFile", "arguments": "{}"},
				map[string]any{"type": "function_call", "name": "clnt_unknown"},
				map[string]any{"type": "function_call", "name": "shell"},
				map[string]any{"type": "custom_tool_call", "name": "invented"},
				map[string]any{"type": "web_search_call", "status": "completed"},
				map[string]any{"type": "shell_call", "status": "completed"},
			},
			"usage": map[string]any{"total_tokens": 8},
		}
		if envelope {
			payload = map[string]any{"response": payload}
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		out := decodeFilteredResponse(t, RenameResponseTool(body, Responses, filteredResponseRenames()))
		if envelope {
			out = jsonutil.MapAt(out, "response")
		}
		output := jsonutil.SliceAt(out, "output")
		if len(output) != 6 || jsonutil.IntAt(out, "usage", "total_tokens") != 8 {
			t.Fatalf("filtered Responses output = %v", out)
		}
		for _, index := range []int{1, 2, 3} {
			if jsonutil.StringAt(output[index].(map[string]any), "name") != "ReadFile" {
				t.Fatalf("allowed tool was not restored: %v", output[index])
			}
		}
		if jsonutil.StringAt(output[4].(map[string]any), "type") != "web_search_call" || jsonutil.StringAt(output[5].(map[string]any), "type") != "shell_call" {
			t.Fatalf("builtin was filtered: %v", output)
		}
	}
}

func TestResponseFilterAnthropicMixedContent(t *testing.T) {
	body := []byte(`{"type":"message","content":[{"type":"text","text":"kept"},{"type":"tool_use","name":"read","input":{}},{"type":"tool_use","name":"clnt_unknown","input":{}},{"type":"tool_use","name":"shell","input":{}},{"type":"tool_use","name":"invented","input":{}},{"type":"server_tool_use","name":"web_search","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":9}}`)
	out := decodeFilteredResponse(t, RenameResponseTool(body, Anthropic, filteredResponseRenames()))
	content := jsonutil.SliceAt(out, "content")
	if len(content) != 3 || jsonutil.StringAt(content[1].(map[string]any), "name") != "ReadFile" || jsonutil.StringAt(content[2].(map[string]any), "type") != "server_tool_use" || out["stop_reason"] != "tool_use" || jsonutil.IntAt(out, "usage", "input_tokens") != 9 {
		t.Fatalf("mixed Anthropic response = %v", out)
	}
}

func TestResponseFilterAnthropicServerToolPreservesToolStop(t *testing.T) {
	for _, empty := range []bool{false, true} {
		r := filteredResponseRenames()
		if empty {
			r = NewToolRenames()
			r.EnableResponseFilter()
		}
		for _, clientCall := range []string{"", `,{"type":"tool_use","name":"invented","input":{}}`} {
			body := []byte(`{"type":"message","content":[{"type":"text","text":"kept"},{"type":"server_tool_use","id":"server_1","name":"web_search","input":{"query":"test"}}` + clientCall + `],"stop_reason":"tool_use","usage":{"input_tokens":9}}`)
			out := decodeFilteredResponse(t, RenameResponseTool(body, Anthropic, r))
			content := jsonutil.SliceAt(out, "content")
			if len(content) != 2 || jsonutil.StringAt(content[1].(map[string]any), "type") != "server_tool_use" || jsonutil.StringAt(content[1].(map[string]any), "name") != "web_search" || out["stop_reason"] != "tool_use" || jsonutil.IntAt(out, "usage", "input_tokens") != 9 {
				t.Fatalf("server tool stop must survive client filtering (empty=%v): %v", empty, out)
			}
		}
	}
}

func TestResponseFilterAllRemovedAndNoClientTools(t *testing.T) {
	for _, empty := range []bool{false, true} {
		r := filteredResponseRenames()
		if empty {
			r = NewToolRenames()
			r.EnableResponseFilter()
		}
		for _, finish := range []string{"tool_calls", "function_call"} {
			body := []byte(`{"choices":[{"message":{"content":"kept","tool_calls":[{"function":{"name":"invented"}}],"function_call":{"name":"invented"}},"finish_reason":"` + finish + `"}],"usage":{"total_tokens":10}}`)
			out := decodeFilteredResponse(t, RenameResponseTool(body, Chat, r))
			choice := jsonutil.SliceAt(out, "choices")[0].(map[string]any)
			message := jsonutil.MapAt(choice, "message")
			if _, ok := message["tool_calls"]; ok {
				t.Fatalf("all-removed tool_calls retained: %v", out)
			}
			if _, ok := message["function_call"]; ok || choice["finish_reason"] != "stop" || message["content"] != "kept" || jsonutil.IntAt(out, "usage", "total_tokens") != 10 {
				t.Fatalf("all-removed Chat response = %v", out)
			}
		}
		out := decodeFilteredResponse(t, RenameResponseTool([]byte(`{"content":[{"type":"text","text":"kept"},{"type":"tool_use","name":"invented","input":{}}],"stop_reason":"tool_use"}`), Anthropic, r))
		if len(jsonutil.SliceAt(out, "content")) != 1 || out["stop_reason"] != "end_turn" {
			t.Fatalf("all-removed Anthropic response = %v", out)
		}
		out = decodeFilteredResponse(t, RenameResponseTool([]byte(`{"output":[{"type":"function_call","name":"invented"}]}`), Responses, r))
		if output := jsonutil.SliceAt(out, "output"); output == nil || len(output) != 0 {
			t.Fatalf("all-removed Responses output = %v", out)
		}
	}
}

func TestResponseFilterDisabledPassesUnknownToolsThrough(t *testing.T) {
	for protocol, body := range map[Protocol]string{
		Chat:      `{"choices":[{"message":{"tool_calls":[{"function":{"name":"invented"}}]},"finish_reason":"tool_calls"}]}`,
		Responses: `{"output":[{"type":"function_call","name":"invented"}]}`,
		Anthropic: `{"content":[{"type":"tool_use","name":"invented","input":{}}],"stop_reason":"tool_use"}`,
	} {
		for _, r := range []*ToolRenames{nil, NewToolRenames(), shellRenames()} {
			if got := RenameResponseTool([]byte(body), protocol, r); string(got) != body {
				t.Errorf("%s rename-only context changed unknown tool: %s", protocol, got)
			}
		}
	}
}

func TestResponseFilterLeavesSSEFragmentsToStatefulReader(t *testing.T) {
	for protocol, body := range map[Protocol]string{
		Chat:      `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"invented"}}]},"finish_reason":"tool_calls"}]}`,
		Responses: `{"type":"response.output_item.added","item":{"type":"function_call","name":"invented"}}`,
		Anthropic: `{"type":"content_block_start","content_block":{"type":"tool_use","name":"invented","input":{}}}`,
	} {
		if got := RenameResponseTool([]byte(body), protocol, filteredResponseRenames()); string(got) != body {
			t.Errorf("%s standalone SSE event was filtered: %s", protocol, got)
		}
	}
	body := []byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"invented"}},{"function":{"name":"clnt_known"}}]},"finish_reason":"tool_calls"}]}`)
	out := decodeFilteredResponse(t, renameResponseTool(body, Chat, filteredResponseRenames(), false))
	calls := jsonutil.SliceAt(jsonutil.MapAt(jsonutil.SliceAt(out, "choices")[0].(map[string]any), "message"), "tool_calls")
	if len(calls) != 2 || jsonutil.StringAt(calls[1].(map[string]any), "function", "name") != "ReadFile" {
		t.Fatalf("filter=false must restore names without dropping tools: %v", out)
	}
}

func TestResponseToolIdentityBindingStillMapsOfficialInput(t *testing.T) {
	r := NewToolRenames()
	r.Add("edit", "edit")
	r.BindOfficialArgs("edit", map[string]string{"filePath": "file_path"})
	body := []byte(`{"content":[{"type":"tool_use","name":"edit","input":{"filePath":"a.go"}}]}`)
	out := decodeFilteredResponse(t, RenameResponseTool(body, Anthropic, r))
	tool := jsonutil.SliceAt(out, "content")[0].(map[string]any)
	if got := jsonutil.MapAt(tool, "input"); !reflect.DeepEqual(got, map[string]any{"file_path": "a.go"}) {
		t.Fatalf("identity official arguments = %v", got)
	}
}

func TestResponseToolSameNameOfficialArgumentsAndAliasArguments(t *testing.T) {
	r := NewToolRenames()
	r.Add("edit", "clnt_edit")
	r.BindOfficial("edit", "edit")
	r.BindOfficialArgs("edit", map[string]string{"filePath": "file_path", "oldString": "old_string", "replaceAll": ""})
	r.EnableResponseFilter()
	const arguments = `{"filePath":"a.go","oldString":"before","replaceAll":true,"extra":1}`
	for _, protocol := range []Protocol{Chat, Responses, Anthropic} {
		for _, name := range []string{"edit", "clnt_edit"} {
			tool := map[string]any{"name": name, "arguments": arguments}
			var payload map[string]any
			switch protocol {
			case Chat:
				payload = map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"function": tool}}}}}}
			case Responses:
				tool["type"] = "function_call"
				payload = map[string]any{"output": []any{tool}}
			case Anthropic:
				tool["type"] = "tool_use"
				tool["input"] = decodeFilteredResponse(t, []byte(arguments))
				delete(tool, "arguments")
				payload = map[string]any{"content": []any{tool}}
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			out := decodeFilteredResponse(t, RenameResponseTool(body, protocol, r))
			switch protocol {
			case Chat:
				choice := jsonutil.SliceAt(out, "choices")[0].(map[string]any)
				tool = jsonutil.MapAt(jsonutil.SliceAt(jsonutil.MapAt(choice, "message"), "tool_calls")[0].(map[string]any), "function")
			case Responses:
				tool = jsonutil.SliceAt(out, "output")[0].(map[string]any)
			case Anthropic:
				tool = jsonutil.SliceAt(out, "content")[0].(map[string]any)
			}
			if tool["name"] != "edit" {
				t.Fatalf("%s tool name = %v", protocol, tool)
			}
			var got map[string]any
			if protocol == Anthropic {
				got = jsonutil.MapAt(tool, "input")
			} else {
				got = decodeFilteredResponse(t, []byte(jsonutil.StringAt(tool, "arguments")))
			}
			want := map[string]any{"file_path": "a.go", "old_string": "before", "extra": float64(1)}
			if name == "clnt_edit" {
				want = decodeFilteredResponse(t, []byte(arguments))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s arguments = %v; want %v", protocol, name, got, want)
			}
		}
	}
}
