package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

// These tests obtain the response context from gateway request shaping rather
// than manually registering names or enabling the protocol-layer filter.
func TestShapedResponseFilter(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, v1 := range []bool{false, true} {
			for _, scenario := range []string{"no_client_tools", "official_edit", "hashed_edit"} {
				t.Run(fmt.Sprintf("%s/v1=%v/%s", protocol, v1, scenario), func(t *testing.T) {
					payload := map[string]any{}
					if scenario != "no_client_tools" {
						payload["tools"] = []any{responseFilterClientEdit(protocol)}
					}
					renames, shaped := renameAndCompleteTools(payload, protocol, v1, "")
					if !shaped || !renames.RestrictsResponses() || renames.Empty() {
						t.Fatal("shaped request did not retain an active response filter")
					}
					if !responseFilterHasTool(payload, protocol, "edit") || (!v1 && !responseFilterHasTool(payload, protocol, "question")) {
						t.Fatal("upstream fixture must expose injected official edit and question")
					}

					name := "edit"
					arguments := `{"path":"a.go","oldString":"before","newString":"after","replaceAll":true}`
					if v1 {
						arguments = `{"filePath":"a.go","oldString":"before","newString":"after","replaceAll":true}`
					}
					wantArgs := map[string]any{"file_path": "a.go", "old_string": "before", "new_string": "after", "replace_all": true}
					if scenario == "hashed_edit" {
						var ok bool
						name, ok = renames.Wire("edit")
						if !ok || !strings.HasPrefix(name, wire.ClientToolPrefix) {
							t.Fatalf("client edit did not get an opaque alias: %q", name)
						}
						// Official-looking keys are deliberately included: alias calls
						// already use the client's schema and must not be remapped.
						arguments = `{"file_path":"a.go","old_string":"before","new_string":"after","path":"keep","replaceAll":true}`
						wantArgs = responseFilterDecode(t, []byte(arguments))
					}
					calls := []map[string]any{
						{"id": "drop_question", "name": "question", "arguments": `{"questions":[]}`},
						{"id": "edit_call", "name": name, "arguments": arguments},
					}
					body := responseFilterDocument(protocol, calls)
					got := wire.RenameResponseTool(responseFilterJSON(body), protocol, renames)
					responseFilterAssert(t, protocol, got, scenario != "no_client_tools", wantArgs)

					stream := responseFilterStream(protocol, calls)
					filtered, err := io.ReadAll(wire.RenameStreamReader(strings.NewReader(stream), protocol, renames))
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(filtered), "drop_question") || strings.Contains(string(filtered), `"name":"question"`) {
						t.Fatalf("unbound injected question leaked into stream: %s", filtered)
					}
					collapsed, err := wire.CollapseStream(strings.NewReader(string(filtered)), protocol, "test-model")
					if err != nil {
						t.Fatalf("filtered stream did not complete: %v\n%s", err, filtered)
					}
					responseFilterAssert(t, protocol, collapsed, scenario != "no_client_tools", wantArgs)
				})
			}
		}
	}
}

func TestShapedResponseFilterPreservesBuiltins(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Responses, wire.Anthropic} {
		t.Run(string(protocol), func(t *testing.T) {
			renames, _ := renameAndCompleteTools(map[string]any{}, protocol, false, "")
			var document map[string]any
			var stream string
			if protocol == wire.Responses {
				builtin := map[string]any{"id": "builtin", "type": "web_search_call", "status": "completed"}
				document = map[string]any{"output": []any{builtin}, "status": "completed"}
				stream = responseFilterSSE(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": builtin}) +
					responseFilterSSE(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": builtin}) +
					responseFilterSSE(map[string]any{"type": "response.completed", "response": document})
			} else {
				builtin := map[string]any{"id": "builtin", "type": "server_tool_use", "name": "web_search", "input": map[string]any{"query": "q"}}
				document = map[string]any{"content": []any{builtin}, "stop_reason": "tool_use"}
				stream = responseFilterSSE(map[string]any{"type": "content_block_start", "index": 0, "content_block": builtin}) +
					responseFilterSSE(map[string]any{"type": "content_block_stop", "index": 0}) +
					responseFilterSSE(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}}) +
					responseFilterSSE(map[string]any{"type": "message_stop"})
			}
			body := responseFilterJSON(document)
			if got := wire.RenameResponseTool(body, protocol, renames); !reflect.DeepEqual(responseFilterDecode(t, got), document) {
				t.Fatalf("builtin document changed: %s", got)
			}
			got, err := io.ReadAll(wire.RenameStreamReader(strings.NewReader(stream), protocol, renames))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), `"id":"builtin"`) || (protocol == wire.Anthropic && !strings.Contains(string(got), `"stop_reason":"tool_use"`)) {
				t.Fatalf("builtin stream or its ending changed: %s", got)
			}
		})
	}
}

// Exercise the actual HTTP handler as well: the free key tier shapes the
// outbound request, stores its renames in requestShape, and reads that context
// back for both a streaming response and a collapsed non-streaming response.
func TestShapedResponseFilterHTTPHandler(t *testing.T) {
	const model = "deepseek-v4.1-flash-free"
	for _, clientEdit := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("edit=%v/stream=%v", clientEdit, stream), func(t *testing.T) {
				outboundRequests := make(chan map[string]any, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/chat/completions" {
						t.Errorf("upstream path = %q", r.URL.Path)
					}
					var outbound map[string]any
					if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					outboundRequests <- outbound
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, responseFilterStream(wire.Chat, []map[string]any{
						{"id": "drop_question", "name": "question", "arguments": `{"questions":[]}`},
						{"id": "edit_call", "name": "edit", "arguments": `{"path":"a.go","oldString":"before","newString":"after","replaceAll":true}`},
					}))
				}))
				defer upstream.Close()
				cfg := config.Config{
					ServerKeys: []string{"local-test-key"}, ZenKeys: []string{"upstream-test-key"},
					Upstream: config.UpstreamConfig{Zen: upstream.URL},
					Retry:    config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 5},
					Models:   config.ModelsConfig{Protocols: map[string]string{model: string(wire.Chat)}},
				}
				gateway, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				gateway.catalog.Replace([]string{model}, nil)
				payload := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "edit a.go"}}, "stream": stream}
				if clientEdit {
					payload["tools"] = []any{responseFilterClientEdit(wire.Chat)}
				}
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(responseFilterJSON(payload))))
				request.Header.Set("Authorization", "Bearer local-test-key")
				recorder := httptest.NewRecorder()
				gateway.Handler().ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("handler status = %d: %s", recorder.Code, recorder.Body.String())
				}
				outbound := <-outboundRequests
				if outbound["stream"] != true || !responseFilterHasTool(outbound, wire.Chat, "edit") || !responseFilterHasTool(outbound, wire.Chat, "question") {
					t.Fatalf("upstream request was not shaped: %v", outbound)
				}
				body := recorder.Body.Bytes()
				if stream {
					body, err = wire.CollapseStream(strings.NewReader(string(body)), wire.Chat, model)
					if err != nil {
						t.Fatalf("handler stream did not complete: %v\n%s", err, recorder.Body.String())
					}
				}
				responseFilterAssert(t, wire.Chat, body, clientEdit, map[string]any{"file_path": "a.go", "old_string": "before", "new_string": "after", "replace_all": true})
			})
		}
	}
}

func responseFilterClientEdit(protocol wire.Protocol) map[string]any {
	parameters := map[string]any{"type": "object", "properties": map[string]any{
		"file_path": map[string]any{"type": "string"}, "old_string": map[string]any{"type": "string"},
		"new_string": map[string]any{"type": "string"}, "replace_all": map[string]any{"type": "boolean"},
	}, "additionalProperties": false}
	switch protocol {
	case wire.Chat:
		return map[string]any{"type": "function", "function": map[string]any{"name": "edit", "description": "Client edit", "parameters": parameters}}
	case wire.Responses:
		return map[string]any{"type": "function", "name": "edit", "description": "Client edit", "parameters": parameters}
	default:
		return map[string]any{"name": "edit", "description": "Client edit", "input_schema": parameters}
	}
}

func responseFilterHasTool(payload map[string]any, protocol wire.Protocol, name string) bool {
	tools, _ := payload["tools"].([]any)
	for _, raw := range tools {
		entry, _ := raw.(map[string]any)
		if got, ok := renamableToolName(protocol, entry); ok && got == name {
			return true
		}
	}
	return false
}

func responseFilterDocument(protocol wire.Protocol, calls []map[string]any) map[string]any {
	items := make([]any, 0, len(calls))
	for _, call := range calls {
		switch protocol {
		case wire.Chat:
			items = append(items, map[string]any{"id": call["id"], "type": "function", "function": map[string]any{"name": call["name"], "arguments": call["arguments"]}})
		case wire.Responses:
			items = append(items, map[string]any{"id": call["id"], "call_id": call["id"], "type": "function_call", "name": call["name"], "arguments": call["arguments"], "status": "completed"})
		case wire.Anthropic:
			var input map[string]any
			_ = json.Unmarshal([]byte(call["arguments"].(string)), &input)
			items = append(items, map[string]any{"id": call["id"], "type": "tool_use", "name": call["name"], "input": input})
		}
	}
	switch protocol {
	case wire.Chat:
		return map[string]any{"id": "chatcmpl-test", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "kept", "tool_calls": items}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"total_tokens": 7}}
	case wire.Responses:
		return map[string]any{"id": "resp-test", "output": items, "status": "completed", "usage": map[string]any{"total_tokens": 7}}
	default:
		return map[string]any{"id": "msg-test", "type": "message", "content": items, "stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 3, "output_tokens": 4}}
	}
}

func responseFilterStream(protocol wire.Protocol, calls []map[string]any) string {
	var out strings.Builder
	write := func(event map[string]any) { out.WriteString(responseFilterSSE(event)) }
	if protocol == wire.Anthropic {
		write(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-test", "type": "message", "role": "assistant", "model": "test-model", "content": []any{}, "usage": map[string]any{"input_tokens": 3}}})
	}
	for index, call := range calls {
		arguments := call["arguments"].(string)
		mid := len(arguments) / 2
		switch protocol {
		case wire.Chat:
			write(map[string]any{"id": "chatcmpl-test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": call["id"], "type": "function", "function": map[string]any{"name": call["name"], "arguments": arguments[:mid]}}}}}}})
			write(map[string]any{"id": "chatcmpl-test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": arguments[mid:]}}}}}}})
		case wire.Responses:
			write(map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"id": call["id"], "call_id": call["id"], "type": "function_call", "name": call["name"], "arguments": ""}})
			for _, fragment := range []string{arguments[:mid], arguments[mid:]} {
				write(map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": call["id"], "delta": fragment})
			}
			write(map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": call["id"], "arguments": arguments})
			write(map[string]any{"type": "response.output_item.done", "output_index": index, "item": map[string]any{"id": call["id"], "call_id": call["id"], "type": "function_call", "name": call["name"], "arguments": arguments}})
		case wire.Anthropic:
			write(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": call["id"], "name": call["name"], "input": map[string]any{}}})
			for _, fragment := range []string{arguments[:mid], arguments[mid:]} {
				write(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": fragment}})
			}
			write(map[string]any{"type": "content_block_stop", "index": index})
		}
	}
	switch protocol {
	case wire.Chat:
		write(map[string]any{"id": "chatcmpl-test", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}})
		out.WriteString("data: [DONE]\n\n")
	case wire.Responses:
		write(map[string]any{"type": "response.completed", "response": responseFilterDocument(protocol, calls)})
	case wire.Anthropic:
		write(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 4}})
		write(map[string]any{"type": "message_stop"})
	}
	return out.String()
}

func responseFilterAssert(t *testing.T, protocol wire.Protocol, body []byte, keepEdit bool, wantArgs map[string]any) {
	t.Helper()
	payload := responseFilterDecode(t, body)
	var tools []any
	var finish any
	switch protocol {
	case wire.Chat:
		choice := payload["choices"].([]any)[0].(map[string]any)
		message := choice["message"].(map[string]any)
		tools, _ = message["tool_calls"].([]any)
		finish = choice["finish_reason"]
	case wire.Responses:
		tools, _ = payload["output"].([]any)
		finish = payload["status"]
	case wire.Anthropic:
		tools, _ = payload["content"].([]any)
		finish = payload["stop_reason"]
	}
	wantCount := 0
	if keepEdit {
		wantCount = 1
	}
	if len(tools) != wantCount {
		t.Fatalf("response tool count = %d; want %d: %s", len(tools), wantCount, body)
	}
	wantFinish := "completed"
	if protocol == wire.Chat {
		wantFinish = "stop"
		if keepEdit {
			wantFinish = "tool_calls"
		}
	} else if protocol == wire.Anthropic {
		wantFinish = "end_turn"
		if keepEdit {
			wantFinish = "tool_use"
		}
	}
	if finish != wantFinish {
		t.Fatalf("response ending = %v; want %s: %s", finish, wantFinish, body)
	}
	if !keepEdit {
		return
	}
	tool := tools[0].(map[string]any)
	if protocol == wire.Chat {
		tool = tool["function"].(map[string]any)
	}
	if tool["name"] != "edit" {
		t.Fatalf("restored name = %v; want edit: %s", tool["name"], body)
	}
	var arguments map[string]any
	if protocol == wire.Anthropic {
		arguments, _ = tool["input"].(map[string]any)
	} else {
		arguments = responseFilterDecode(t, []byte(tool["arguments"].(string)))
	}
	if !reflect.DeepEqual(arguments, wantArgs) {
		t.Fatalf("arguments = %v; want %v: %s", arguments, wantArgs, body)
	}
}

func responseFilterDecode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("invalid response JSON: %v: %s", err, body)
	}
	return payload
}

func responseFilterJSON(value any) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}

func responseFilterSSE(event map[string]any) string {
	prefix := ""
	if name, ok := event["type"].(string); ok {
		prefix = "event: " + name + "\n"
	}
	return prefix + "data: " + string(responseFilterJSON(event)) + "\n\n"
}
