package protocol

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"opencode2api/internal/jsonutil"
)

func shellBridgeRenames() *ToolRenames {
	r := NewToolRenames()
	r.Add("exec_command", ClientToolWireName("exec_command", nil))
	r.BindOfficial("shell", "exec_command")
	r.BindOfficialArgs("shell", map[string]string{"command": "cmd", "timeout": "", "background": ""})
	r.EnableResponseFilter()
	r.EnableShellBridge("exec_command")
	return r
}

func bridgeCommand(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(args)
	call := map[string]any{"name": name, "arguments": string(b)}
	if err := shellBridgeRenames().adaptToolCall(call, false); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(call["arguments"].(string)), &out); err != nil {
		t.Fatal(err)
	}
	return out["cmd"].(string)
}

func runBridgeCommand(t *testing.T, name string, args map[string]any, dir string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// The POSIX bridge is disabled on Windows hosts, and an MSYS shell
		// collapses the backslashes in the JSON argument before python sees it.
		t.Skip("POSIX command bridge")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("client helper requires a POSIX shell")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("client helper requires python3")
	}
	if name != "read" {
		if _, err := exec.LookPath("rg"); err != nil {
			t.Skip("client helper requires rg")
		}
	}
	cmd := exec.Command("sh", "-c", bridgeCommand(t, name, args))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestFilesystemBridgeExecutesClientSemantics(t *testing.T) {
	dir := t.TempDir()
	// A filename must remain data even when it looks like shell syntax.
	path := filepath.Join(dir, "a ' $(touch SENTINEL) [x].js")
	if err := os.WriteFile(path, []byte("first\nNeedle [x]\nlast\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden.js"), []byte("needle [x]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runBridgeCommand(t, "read", map[string]any{"path": path, "offset": 2, "limit": 1}, dir)
	if err != nil || out != "2: Needle [x]\n" {
		t.Fatalf("read: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "SENTINEL")); !os.IsNotExist(err) {
		t.Fatal("filename executed shell syntax")
	}
	out, err = runBridgeCommand(t, "read", map[string]any{"path": path, "limit": 0}, dir)
	if err != nil || out != "" {
		t.Fatalf("zero limit: %q %v", out, err)
	}
	out, err = runBridgeCommand(t, "glob", map[string]any{"pattern": "**/*.js", "path": dir}, dir)
	if err != nil || !strings.Contains(out, filepath.Base(path)) || strings.Contains(out, ".hidden.js") {
		t.Fatalf("glob: %q %v", out, err)
	}
	out, err = runBridgeCommand(t, "glob", map[string]any{"pattern": "**/*.js", "path": dir, "hidden": true, "limit": 1}, dir)
	if err != nil || strings.Count(out, "\n") != 1 || !strings.Contains(out, ".hidden.js") {
		t.Fatalf("hidden/limit glob: %q %v", out, err)
	}
	out, err = runBridgeCommand(t, "grep", map[string]any{"pattern": "needle [x]", "path": dir, "literal": true, "caseSensitive": false, "include": "*.js", "limit": 1}, dir)
	if err != nil || !strings.Contains(out, ":2: Needle [x]") || strings.Count(out, "\n") != 1 {
		t.Fatalf("grep: %q %v", out, err)
	}
	out, err = runBridgeCommand(t, "grep", map[string]any{"pattern": "never matches", "path": dir}, dir)
	if err != nil || out != "" {
		t.Fatalf("empty grep: %q %v", out, err)
	}
}

func TestReadBridgeDirectoriesCapAndBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte(strings.Repeat("line\n", 2005)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "b"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := runBridgeCommand(t, "read", map[string]any{"path": dir, "offset": 2, "limit": 1}, dir)
	if err != nil || out != "b/\n" {
		t.Fatalf("directory: %q %v", out, err)
	}
	out, err = runBridgeCommand(t, "read", map[string]any{"path": filepath.Join(dir, "a"), "limit": 3000}, dir)
	if err != nil || strings.Count(out, "\n") != 2000 || !strings.HasSuffix(out, "2000: line\n") {
		t.Fatalf("read cap: %v", err)
	}
	for _, content := range []string{"%PDF-1.7\n", "binary\x00data", "\x89PNG\r\n"} {
		path := filepath.Join(dir, "binary")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		out, err = runBridgeCommand(t, "read", map[string]any{"path": path}, dir)
		if err == nil || !strings.Contains(out, "requires a dedicated client tool") {
			t.Fatalf("binary read: %q %v", out, err)
		}
	}
}

func TestBridgeRejectsUnsupportedOrInvalidArguments(t *testing.T) {
	for _, call := range []map[string]any{
		{"name": "execute", "arguments": `{"code":"return 1"}`},
		{"name": "read", "arguments": `{"path":"x","offset":0}`},
		{"name": "read", "arguments": `{"path":"x","limit":-1}`},
		{"name": "read", "arguments": `{"path":"x","limit":1.5}`},
		{"name": "read", "arguments": `{"path":"x","extra":true}`},
		{"name": "read", "arguments": `{"path":"x"`},
		{"name": "glob", "arguments": `{"pattern":"*.js","limit":0}`},
		{"name": "grep", "arguments": `{"pattern":"x","literal":"true"}`},
	} {
		if err := shellBridgeRenames().adaptToolCall(call, false); err == nil {
			t.Fatalf("accepted %v", call)
		}
	}
	r := shellBridgeRenames()
	alias, _ := r.Wire("exec_command")
	call := map[string]any{"name": alias, "arguments": `{"cmd":"pwd","path":"opaque"}`}
	if err := r.adaptToolCall(call, false); err != nil || call["arguments"] != `{"cmd":"pwd","path":"opaque"}` {
		t.Fatalf("alias changed: %v %v", call, err)
	}
	r.Add("Read", ClientToolWireName("Read", nil))
	r.BindOfficial("read", "Read")
	call = map[string]any{"name": "read", "arguments": `{"path":"x"}`}
	if err := r.adaptToolCall(call, false); err != nil || call["name"] != "read" {
		t.Fatal("native read was replaced")
	}
}

func bridgeFixture(t *testing.T, protocol Protocol, name, args string) []byte {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"id": "fixture", "model": "m", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}, "finish_reason": "tool_calls"}}})
	out, err := ConvertResponse(Chat, protocol, b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBridgeCompleteResponsesAllProtocols(t *testing.T) {
	for _, protocol := range []Protocol{Chat, Responses, Anthropic} {
		r := shellBridgeRenames()
		body := bridgeFixture(t, protocol, "read", `{"path":"a.js","offset":2,"limit":1}`)
		adapted, err := AdaptResponseTools(body, protocol, r)
		if err != nil {
			t.Fatal(err)
		}
		adapted = RenameResponseTool(adapted, protocol, r)
		chat, err := ConvertResponse(protocol, Chat, adapted)
		if err != nil {
			t.Fatal(err)
		}
		out := decodeFilteredResponse(t, chat)
		call := jsonutil.SliceAt(jsonutil.MapAt(jsonutil.SliceAt(out, "choices")[0].(map[string]any), "message"), "tool_calls")[0].(map[string]any)
		if call["id"] != "call_1" || jsonutil.StringAt(call, "function", "name") != "exec_command" || !strings.Contains(jsonutil.StringAt(call, "function", "arguments"), "python3") {
			t.Fatalf("%s: %s", protocol, chat)
		}
		if _, err := AdaptResponseTools(bridgeFixture(t, protocol, "execute", `{"code":"1"}`), protocol, r); err == nil {
			t.Fatalf("%s silently accepted execute", protocol)
		}
	}
}

func TestBridgeStreamFragmentsAndMixedCalls(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"r","arguments":"{\"pa"}},{"index":1,"id":"call_glob","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.js\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"ea","arguments":"th\":\"a ' [x].js\",\"offset\":2"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"d","arguments":",\"limit\":1}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var input strings.Builder
	for _, frame := range frames {
		input.WriteString("data: " + frame + "\n\n")
	}
	input.WriteString("data: [DONE]\n\n")
	body, err := CollapseStream(RenameStreamReader(strings.NewReader(input.String()), Chat, shellBridgeRenames()), Chat, "m")
	if err != nil {
		t.Fatal(err)
	}
	out := decodeFilteredResponse(t, body)
	calls := jsonutil.SliceAt(jsonutil.MapAt(jsonutil.SliceAt(out, "choices")[0].(map[string]any), "message"), "tool_calls")
	if len(calls) != 2 {
		t.Fatalf("lost mixed calls: %s", body)
	}
	for i, id := range []string{"call_read", "call_glob"} {
		call := calls[i].(map[string]any)
		if call["id"] != id || jsonutil.StringAt(call, "function", "name") != "exec_command" {
			t.Fatalf("lost call identity: %s", body)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(jsonutil.StringAt(call, "function", "arguments")), &args); err != nil || args["cmd"] == nil {
			t.Fatalf("incomplete arguments: %s", body)
		}
	}
	bad := strings.Replace(input.String(), `"name":"glob"`, `"name":"execute"`, 1)
	w := httptest.NewRecorder()
	_, _, err = TranscodeStream(context.Background(), w, RenameStreamReader(strings.NewReader(bad), Chat, shellBridgeRenames()), Chat, Responses, "m")
	if err == nil || !strings.Contains(w.Body.String(), "response.failed") || strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "function_call\"") || strings.Contains(w.Body.String(), "execute") {
		t.Fatalf("unsupported mixed call became success: %s %v", w.Body.String(), err)
	}
}

func TestBridgeNativeStreams(t *testing.T) {
	for _, protocol := range []Protocol{Responses, Anthropic} {
		w := httptest.NewRecorder()
		emitter := bridgeStreamEmitter(w, w, protocol, "m")
		for _, event := range []bridgeStreamEvent{{Kind: "start", ResponseID: "fixture"}, {Kind: "tool_start", ToolKey: "0", ToolID: "call_1", ToolName: "read"}, {Kind: "tool_delta", ToolKey: "0", Text: `{"path":`}, {Kind: "tool_delta", ToolKey: "0", Text: `"a.js"}`}, {Kind: "finish", Stop: "tool_calls"}, {Kind: "done"}} {
			if err := emitter.Emit(event); err != nil {
				t.Fatal(err)
			}
		}
		body, err := CollapseStream(RenameStreamReader(strings.NewReader(w.Body.String()), protocol, shellBridgeRenames()), protocol, "m")
		if err != nil {
			t.Fatalf("%s: %v", protocol, err)
		}
		chat, err := ConvertResponse(protocol, Chat, body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(chat), "exec_command") || !strings.Contains(string(chat), "python3") || !strings.Contains(string(chat), "call_1") {
			t.Fatalf("%s lost call: %s", protocol, chat)
		}
		bad := strings.ReplaceAll(w.Body.String(), `"name":"read"`, `"name":"execute"`)
		if _, err := io.ReadAll(RenameStreamReader(strings.NewReader(bad), protocol, shellBridgeRenames())); err == nil {
			t.Fatalf("%s silently dropped unavailable tool", protocol)
		}
	}
}

func TestBridgeFullMessageAndMissingNativeMetadata(t *testing.T) {
	fixture := decodeFilteredResponse(t, bridgeFixture(t, Chat, "read", `{"path":"a.js"}`))
	fixture["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] = "kept text"
	b, _ := json.Marshal(fixture)
	body, err := CollapseStream(RenameStreamReader(strings.NewReader("data: "+string(b)+"\n\ndata: [DONE]\n\n"), Chat, shellBridgeRenames()), Chat, "m")
	if err != nil || !strings.Contains(string(body), "kept text") || !strings.Contains(string(body), "exec_command") {
		t.Fatalf("full message lost text/tools: %s %v", body, err)
	}
	item := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read", "arguments": `{"path":"a.js"}`}
	var input strings.Builder
	for _, event := range []map[string]any{{"type": "response.output_item.done", "output_index": 0, "item": item}, {"type": "response.completed", "response": map[string]any{"id": "fixture", "status": "completed", "output": []any{item}}}} {
		b, _ := json.Marshal(event)
		input.WriteString("data: " + string(b) + "\n\n")
	}
	body, err = CollapseStream(RenameStreamReader(strings.NewReader(input.String()), Responses, shellBridgeRenames()), Responses, "m")
	if err != nil || !strings.Contains(string(body), "python3") || !strings.Contains(string(body), "call_1") {
		t.Fatalf("native done without start: %s %v", body, err)
	}
	orphan := `data: {"type":"response.function_call_arguments.delta","item_id":"missing","delta":"{}"}` + "\n\n"
	if _, err := io.ReadAll(RenameStreamReader(strings.NewReader(orphan), Responses, shellBridgeRenames())); err == nil || !strings.Contains(err.Error(), "without tool metadata") {
		t.Fatalf("orphan arguments were forwarded: %v", err)
	}
}
