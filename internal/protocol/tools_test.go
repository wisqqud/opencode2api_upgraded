package protocol

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// shellRenames builds the rename set a shaped request produces when the client
// declared exec_command and the gateway bound the official shell tool to it.
func shellRenames() *ToolRenames {
	renames := NewToolRenames()
	renames.Add("exec_command", ClientToolWireName("exec_command", map[string]bool{}))
	renames.BindOfficial("shell", "exec_command")
	renames.BindOfficialArgs("shell", map[string]string{
		"command":    "cmd",
		"workdir":    "workdir",
		"timeout":    "",
		"background": "",
	})
	return renames
}

func TestClientToolWireNameUses24SHA256HexCharacters(t *testing.T) {
	const want = "mcp_aa6e99885a69dcd170429df2"
	used := map[string]bool{}
	if got := ClientToolWireName("exec_command", used); got != want {
		t.Fatalf("ClientToolWireName(exec_command) = %q; want %q", got, want)
	}
	if !used[want] {
		t.Fatalf("wire name %q was not recorded as used", want)
	}
	if got := ClientToolWireName("exec_command", used); got != want+"_2" {
		t.Fatalf("colliding wire name = %q; want %q", got, want+"_2")
	}
	if got := ClientToolWireName("exec_command", nil); got != want {
		t.Fatalf("wire name changed between requests: %q; want %q", got, want)
	}
	renames := NewToolRenames()
	renames.Add("exec_command", want)
	if got, changed := renames.Resolve(want); got != "exec_command" || !changed {
		t.Fatalf("Resolve(%q) = (%q, %v); want (exec_command, true)", want, got, changed)
	}
}

func TestToolRenamesIdentityBindings(t *testing.T) {
	for _, test := range []struct {
		name    string
		renames *ToolRenames
	}{
		{name: "constructor", renames: NewToolRenames()},
		{name: "zero value", renames: &ToolRenames{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			renames := test.renames
			if !renames.Empty() || renames.Count() != 0 {
				t.Fatalf("new mapping: Empty = %v, Count = %d", renames.Empty(), renames.Count())
			}
			renames.Add("read_file", "read_file")
			renames.Add("read_file", "read_file")
			renames.BindOfficial("read_file", "read_file")
			if renames.Empty() || renames.Count() != 0 {
				t.Fatalf("identity binding: Empty = %v, Count = %d; want false, 0", renames.Empty(), renames.Count())
			}
			for method, lookup := range map[string]func(string) (string, bool){
				"Wire": renames.Wire, "Client": renames.Client, "Official": renames.Official,
			} {
				if got, ok := lookup("read_file"); !ok || got != "read_file" {
					t.Fatalf("%s(read_file) = (%q, %v); want (read_file, true)", method, got, ok)
				}
			}
			for name, want := range map[string]string{
				"read_file": "read_file", "READ_FILE": "read_file", "read-file": "read_file",
			} {
				got, changed := renames.Resolve(name)
				if got != want || changed != (name != want) {
					t.Fatalf("Resolve(%q) = (%q, %v); want (%q, %v)", name, got, changed, want, name != want)
				}
			}
			// Conflicts must preserve the first pair in both directions.
			renames.Add("read_file", "another_wire")
			renames.Add("another_client", "read_file")
			if _, ok := renames.Wire("another_client"); ok {
				t.Fatal("identity wire name was assigned to a second client")
			}
			if _, ok := renames.Client("another_wire"); ok {
				t.Fatal("identity client name was assigned a second wire name")
			}
			renames.Add("exec_command", ClientToolWireName("exec_command", nil))
			if renames.Count() != 1 {
				t.Fatalf("mixed identity and rename Count = %d; want 1", renames.Count())
			}
		})
	}
}

func TestIdentityBindingsKeepResponseAndStreamResolutionActive(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("read_file", "read_file")
	renames.BindOfficial("read", "read_file")
	body := []byte(`{"choices":[{"index":0,"message":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":"{\"path\":\"/tmp/test\"}"}}]}}]}`)
	out := RenameResponseTool(body, Chat, renames)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("rewritten response is not JSON: %v", err)
	}
	function := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "read_file" || function["arguments"] != `{"path":"/tmp/test"}` {
		t.Fatalf("identity response = %v; want read_file with unchanged arguments", function)
	}
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_","arguments":""}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"file","arguments":"{\"path\":\"/tmp/test\"}"}}]}}]}`,
		`data: [DONE]`,
	}
	stream, err := io.ReadAll(RenameStreamReader(strings.NewReader(strings.Join(frames, "\n\n")+"\n\n"), Chat, renames))
	if err != nil {
		t.Fatal(err)
	}
	name, arguments := collectStreamCalls(t, string(stream))
	if name != "read_file" || arguments != `{"path":"/tmp/test"}` {
		t.Fatalf("identity stream = (%q, %q); want read_file with unchanged arguments", name, arguments)
	}
}

func TestOfficialOnlyBindingIsNotEmpty(t *testing.T) {
	renames := NewToolRenames()
	renames.BindOfficial("shell", "exec_command")
	if renames.Empty() || renames.Count() != 0 {
		t.Fatalf("official binding: Empty = %v, Count = %d; want false, 0", renames.Empty(), renames.Count())
	}
}

func TestRemapArguments(t *testing.T) {
	mapping := map[string]string{
		"command":    "cmd",
		"workdir":    "workdir",
		"timeout":    "",
		"background": "",
	}
	got := RemapArguments(`{"command":"pwd","workdir":"/tmp","timeout":5000,"background":false}`, mapping)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("arguments %q is not JSON: %v", got, err)
	}
	if decoded["cmd"] != "pwd" {
		t.Fatalf("cmd = %v; want pwd", decoded["cmd"])
	}
	if decoded["workdir"] != "/tmp" {
		t.Fatalf("workdir = %v; want /tmp", decoded["workdir"])
	}
	for _, dropped := range []string{"command", "timeout", "background"} {
		if _, ok := decoded[dropped]; ok {
			t.Fatalf("arguments kept %q: %v", dropped, decoded)
		}
	}
}

func TestRemapArgumentsLeavesOtherJSONAlone(t *testing.T) {
	mapping := map[string]string{"command": "cmd"}
	if got := RemapArguments(`{"cmd":"pwd"}`, mapping); got != `{"cmd":"pwd"}` {
		t.Fatalf("arguments without an official name changed: %q", got)
	}
	if got := RemapArguments(`not json`, mapping); got != `not json` {
		t.Fatalf("non-JSON arguments changed: %q", got)
	}
}

func TestRenameResponseToolRestoresExecCommand(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"pwd\",\"workdir\":\"/tmp\"}"}}]},"finish_reason":"tool_calls"}]}`)
	out := RenameResponseTool(body, Chat, shellRenames())
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	call := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["name"] != "exec_command" {
		t.Fatalf("name = %v; want exec_command", function["name"])
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(function["arguments"].(string)), &arguments); err != nil {
		t.Fatalf("arguments %v: %v", function["arguments"], err)
	}
	if arguments["cmd"] != "pwd" || arguments["workdir"] != "/tmp" {
		t.Fatalf("arguments = %v; want cmd and workdir", arguments)
	}
	if _, ok := arguments["command"]; ok {
		t.Fatalf("arguments kept command: %v", arguments)
	}
}

func TestRenameStreamReaderRestoresExecCommand(t *testing.T) {
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"comm"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"pwd\"}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	stream := strings.Join(frames, "\n\n") + "\n\n"
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(stream), Chat, shellRenames()))
	if err != nil {
		t.Fatal(err)
	}
	name, arguments := collectStreamCalls(t, string(out))
	if name != "exec_command" {
		t.Fatalf("name = %q; want exec_command", name)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		t.Fatalf("arguments %q: %v", arguments, err)
	}
	if decoded["cmd"] != "pwd" {
		t.Fatalf("arguments = %v; want cmd pwd", decoded)
	}
}

// collectStreamCalls reads every tool call out of a rewritten Chat SSE stream.
func collectStreamCalls(t *testing.T, stream string) (string, string) {
	t.Helper()
	name := ""
	var arguments strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := strings.TrimPrefix(line, "data: ")
		if raw == "[DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		for _, rawChoice := range payload["choices"].([]any) {
			choice, _ := rawChoice.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			calls, _ := delta["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				if fragment, ok := function["name"].(string); ok && fragment != "" {
					name = fragment
				}
				if fragment, ok := function["arguments"].(string); ok {
					arguments.WriteString(fragment)
				}
			}
		}
	}
	return name, arguments.String()
}

// TestResolveRebuildsMangledWireName covers a model that imitates a wire name
// from history with an old or misspelled prefix.
func TestResolveRebuildsMangledWireName(t *testing.T) {
	renames := shellRenames()
	wireName, _ := renames.Wire("exec_command")
	digest := strings.TrimPrefix(wireName, ClientToolPrefix)
	for _, mangled := range []string{"clint_" + digest, "clit_" + digest} {
		client, changed := renames.Resolve(mangled)
		if client != "exec_command" || !changed {
			t.Fatalf("Resolve(%q) = (%q, %v); want (exec_command, true)", mangled, client, changed)
		}
	}
}

// TestRenameResponseToolRebuildsMangledWireName keeps the argument rewrite out
// of the way: a mangled name carries client arguments already.
func TestRenameResponseToolRebuildsMangledWireName(t *testing.T) {
	renames := shellRenames()
	wireName, _ := renames.Wire("exec_command")
	digest := strings.TrimPrefix(wireName, ClientToolPrefix)
	body := []byte(`{"choices":[{"index":0,"message":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"clint_` + digest + `","arguments":"{\"cmd\":\"pwd\"}"}}]}}]}`)
	out := RenameResponseTool(body, Chat, renames)
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	function := payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "exec_command" {
		t.Fatalf("name = %v; want exec_command", function["name"])
	}
	if function["arguments"] != `{"cmd":"pwd"}` {
		t.Fatalf("arguments = %v; want unchanged cmd object", function["arguments"])
	}
}

// TestResolveLeavesUnknownHashNameAlone pins the rebuild to registered wire
// names: an official or mangled prefix with an unknown digest stays unchanged.
func TestResolveLeavesUnknownHashNameAlone(t *testing.T) {
	renames := shellRenames()
	for _, name := range []string{
		"mcp_deadbeef0000000000000000",
		"clint_deadbeef0000000000000000",
		"clit_deadbeef0000000000000000",
	} {
		client, changed := renames.Resolve(name)
		if client != name || changed {
			t.Fatalf("Resolve(%q) = (%q, %v); want (%q, false)", name, client, changed, name)
		}
	}
}

// TestResolvePrefersDeclaredLiteralName keeps a client tool whose real name
// looks mangled from being rewritten into another tool.
func TestResolvePrefersDeclaredLiteralName(t *testing.T) {
	renames := shellRenames()
	wireName, _ := renames.Wire("exec_command")
	for _, prefix := range []string{"clint_", "clit_"} {
		literal := prefix + strings.TrimPrefix(wireName, ClientToolPrefix)
		renames.Add(literal, ClientToolWireName(literal, map[string]bool{}))
		client, changed := renames.Resolve(literal)
		if client != literal || changed {
			t.Fatalf("Resolve(%q) = (%q, %v); want (%q, false)", literal, client, changed, literal)
		}
	}
}

// TestRenameStreamReaderRebuildsMangledWireName covers the stream path when the
// mangled name arrives in one frame.
func TestRenameStreamReaderRebuildsMangledWireName(t *testing.T) {
	renames := shellRenames()
	wireName, _ := renames.Wire("exec_command")
	digest := strings.TrimPrefix(wireName, ClientToolPrefix)
	frames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"clint_` + digest + `","arguments":"{\"cmd\":\"pwd\"}"}}]}}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(strings.Join(frames, "\n\n")+"\n\n"), Chat, renames))
	if err != nil {
		t.Fatal(err)
	}
	name, arguments := collectStreamCalls(t, string(out))
	if name != "exec_command" {
		t.Fatalf("name = %q; want exec_command", name)
	}
	if arguments != `{"cmd":"pwd"}` {
		t.Fatalf("arguments = %q; want unchanged cmd object", arguments)
	}
}
