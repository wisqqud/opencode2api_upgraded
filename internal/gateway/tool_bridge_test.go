package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

func bridgeClientTool(protocol wire.Protocol) map[string]any {
	parameters := map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}, "workdir": map[string]any{"type": "string"}}, "required": []any{"cmd"}}
	fn := map[string]any{"name": "exec_command", "description": "Run a client command", "parameters": parameters}
	if protocol == wire.Chat {
		return map[string]any{"type": "function", "function": fn}
	}
	if protocol == wire.Anthropic {
		delete(fn, "parameters")
		fn["input_schema"] = parameters
	} else {
		fn["type"] = "function"
	}
	return fn
}

func TestBridgeShapePreservesOfficialDefinitionsAndExplainsCapabilities(t *testing.T) {
	if hostPlatform() == "win32" {
		t.Skip("POSIX command bridge")
	}
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		payload := map[string]any{"tools": []any{bridgeClientTool(protocol)}}
		r, _ := renameAndCompleteTools(payload, protocol, false, "")
		if !r.HasShellBridge() {
			t.Fatalf("%s did not enable bridge", protocol)
		}
		tools := payload["tools"].([]any)
		canonical := officialToolset(protocol, false, "")
		for i := range canonical {
			if !sameToolDefinition(tools[i].(map[string]any), canonical[i].(map[string]any)) {
				t.Fatal("official fingerprint changed")
			}
		}
		b, _ := json.Marshal(payload)
		if !strings.Contains(string(b), "shell, read") && !strings.Contains(string(b), "glob, grep, read, shell") {
			t.Fatalf("missing executable capabilities: %s", b)
		}
		if !strings.Contains(string(b), "Code Mode catalogs/skills/session-management capabilities are unavailable") {
			t.Fatal("missing runtime limitation")
		}
		if strings.Contains(string(b), "exec_command") {
			t.Fatal("bridge leaked original client tool name")
		}
	}
	tool := bridgeClientTool(wire.Chat)
	tool["function"].(map[string]any)["parameters"].(map[string]any)["required"] = []any{"cmd", "unknown"}
	r, _ := renameAndCompleteTools(map[string]any{"tools": []any{tool}}, wire.Chat, false, "")
	if r.HasShellBridge() {
		t.Fatal("bridge enabled for incompatible command schema")
	}
}

func TestCodexBridgeHTTPResponsesAndFailures(t *testing.T) {
	if hostPlatform() == "win32" {
		t.Skip("POSIX command bridge")
	}
	const model = "deepseek-v4.1-flash-free"
	for _, external := range []wire.Protocol{wire.Chat, wire.Responses} {
		for _, stream := range []bool{false, true} {
			for _, unsupported := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/unsupported=%v", external, stream, unsupported), func(t *testing.T) {
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						calls := []map[string]any{
							{"id": "read_1", "name": "read", "arguments": `{"path":"functions/constants.js"}`},
							{"id": "read_2", "name": "read", "arguments": `{"path":"functions/_middleware.js"}`},
							{"id": "read_3", "name": "read", "arguments": `{"path":"schema.sql"}`},
						}
						if unsupported {
							calls = append(calls, map[string]any{"id": "bad", "name": "execute", "arguments": `{"code":"1"}`})
						}
						_, _ = io.WriteString(w, responseFilterStream(wire.Chat, calls))
					}))
					defer upstream.Close()
					cfg := config.Config{ServerKeys: []string{"local-test-key"}, ZenKeys: []string{"upstream-test-key"}, Upstream: config.UpstreamConfig{Zen: upstream.URL}, Retry: config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 5}, Models: config.ModelsConfig{Protocols: map[string]string{model: string(wire.Chat)}}}
					g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					g.catalog.Replace([]string{model}, nil)
					payload := map[string]any{"model": model, "stream": stream, "tools": []any{bridgeClientTool(external)}}
					path := "/v1/chat/completions"
					if external == wire.Chat {
						payload["messages"] = []any{map[string]any{"role": "user", "content": "分析这个项目"}}
					} else {
						path = "/v1/responses"
						payload["input"] = []any{map[string]any{"role": "user", "content": "分析这个项目"}}
					}
					request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(responseFilterJSON(payload))))
					request.Header.Set("Authorization", "Bearer local-test-key")
					w := httptest.NewRecorder()
					g.Handler().ServeHTTP(w, request)
					if unsupported {
						if !strings.Contains(w.Body.String(), "execute") || (stream && strings.Contains(w.Body.String(), "response.completed")) || (!stream && w.Code != http.StatusBadGateway) {
							t.Fatalf("tool failure reported as success: %d %s", w.Code, w.Body.String())
						}
						if stream {
							if _, err := wire.CollapseStream(strings.NewReader(w.Body.String()), external, model); err == nil {
								t.Fatal("unsupported tool stream completed successfully")
							}
						}
						return
					}
					if w.Code != http.StatusOK {
						t.Fatalf("status=%d %s", w.Code, w.Body.String())
					}
					body := w.Body.Bytes()
					if stream {
						body, err = wire.CollapseStream(strings.NewReader(string(body)), external, model)
						if err != nil {
							t.Fatal(err)
						}
					}
					chat, err := wire.ConvertResponse(external, wire.Chat, body)
					if err != nil {
						t.Fatal(err)
					}
					var out map[string]any
					if err := json.Unmarshal(chat, &out); err != nil {
						t.Fatal(err)
					}
					calls := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
					if len(calls) != 3 || strings.Count(string(chat), "exec_command") != 3 || strings.Count(string(chat), "python3") != 3 {
						t.Fatalf("lost read calls: %s", chat)
					}
				})
			}
		}
	}
}
