package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

func nativeFilterRenames() *ToolRenames {
	renames := NewToolRenames()
	renames.Add("exec_command", "clnt_exec")
	renames.Add("read_file", "clnt_read")
	renames.BindOfficial("shell", "exec_command")
	renames.BindOfficialArgs("shell", map[string]string{"command": "cmd", "timeout": ""})
	renames.EnableResponseFilter()
	return renames
}

func nativeFilterPayload(t *testing.T, raw string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func nativeFilterOne(t *testing.T, filter *nativeToolStreamFilter, raw string) map[string]any {
	t.Helper()
	out := filter.rewrite(nativeFilterPayload(t, raw))
	if len(out) != 1 {
		t.Fatalf("rewrite(%s) emitted %d events; want 1: %v", raw, len(out), out)
	}
	return out[0]
}

func nativeFilterNone(t *testing.T, filter *nativeToolStreamFilter, raw string) {
	t.Helper()
	if out := filter.rewrite(nativeFilterPayload(t, raw)); len(out) != 0 {
		t.Fatalf("rewrite(%s) emitted %v; want no events", raw, out)
	}
}

func TestNativeFilterOnlyConstructsForEnabledNativeProtocol(t *testing.T) {
	for _, renames := range []*ToolRenames{nil, NewToolRenames(), shellRenames()} {
		if filter := newNativeToolStreamFilter(Responses, renames); filter != nil {
			t.Fatal("rename-only request constructed a filter")
		}
	}
	for _, protocol := range []Protocol{Responses, Anthropic} {
		if filter := newNativeToolStreamFilter(protocol, nativeFilterRenames()); filter == nil {
			t.Fatalf("enabled %s filter is nil", protocol)
		}
	}
	if filter := newNativeToolStreamFilter(Chat, nativeFilterRenames()); filter != nil {
		t.Fatal("native filter constructed for Chat")
	}
}

func TestNativeAnthropicMixedToolsAndText(t *testing.T) {
	filter := newNativeToolStreamFilter(Anthropic, nativeFilterRenames())
	nativeFilterOne(t, filter, `{"type":"message_start","message":{"id":"m1","content":[],"usage":{"input_tokens":7}}}`)
	nativeFilterNone(t, filter, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"bad","name":"unknown","input":{}}}`)
	nativeFilterNone(t, filter, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"secret\":1}"}}`)
	nativeFilterNone(t, filter, `{"type":"content_block_stop","index":0}`)
	start := nativeFilterOne(t, filter, `{"type":"content_block_start","index":4,"content_block":{"type":"tool_use","id":"call_4","name":"clnt_read","input":{"path":"/tmp/a"}}}`)
	block := start["content_block"].(map[string]any)
	if start["index"] != float64(4) || block["name"] != "read_file" || block["id"] != "call_4" {
		t.Fatalf("allowed block lost identity: %v", start)
	}
	nativeFilterOne(t, filter, `{"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/b\"}"}}`)
	nativeFilterOne(t, filter, `{"type":"content_block_stop","index":4}`)
	for _, raw := range []string{
		`{"type":"content_block_start","index":8,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":8,"delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"content_block_stop","index":8}`,
		`{"type":"content_block_start","index":9,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":9,"delta":{"type":"thinking_delta","thinking":"consider"}}`,
		`{"type":"content_block_delta","index":9,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`{"type":"content_block_stop","index":9}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`,
		`{"type":"message_stop"}`,
	} {
		want := nativeFilterPayload(t, raw)
		got := filter.rewrite(want)
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("non-tool event changed: %s => %v", raw, got)
		}
	}
	stop := nativeFilterOne(t, filter, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":19}}`)
	if stop["delta"].(map[string]any)["stop_reason"] != "tool_use" || stop["usage"].(map[string]any)["output_tokens"] != float64(19) {
		t.Fatalf("allowed tool stop/usage changed: %v", stop)
	}
}

func TestNativeAnthropicOfficialArgumentsBufferAndMap(t *testing.T) {
	filter := newNativeToolStreamFilter(Anthropic, nativeFilterRenames())
	start := nativeFilterOne(t, filter, `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"c3","name":"shell","input":{"command":"initial","timeout":1,"extra":true}}}`)
	block := start["content_block"].(map[string]any)
	if block["name"] != "exec_command" || !reflect.DeepEqual(block["input"], map[string]any{"cmd": "initial", "extra": true}) {
		t.Fatalf("initial official input was not mapped: %v", block)
	}
	nativeFilterNone(t, filter, `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"},"tag":"keep"}`)
	nativeFilterNone(t, filter, `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"\"pwd\",\"timeout\":2}"}}`)
	out := filter.rewrite(nativeFilterPayload(t, `{"type":"content_block_stop","index":3}`))
	if len(out) != 2 || out[0]["type"] != "content_block_delta" || out[0]["index"] != float64(3) || out[0]["tag"] != "keep" || out[1]["type"] != "content_block_stop" {
		t.Fatalf("mapped fragments not emitted before stop: %v", out)
	}
	if got := out[0]["delta"].(map[string]any)["partial_json"]; got != `{"cmd":"pwd"}` {
		t.Fatalf("mapped partial_json = %v", got)
	}
}

func TestNativeAnthropicNoToolsAndBuiltin(t *testing.T) {
	renames := NewToolRenames()
	renames.EnableResponseFilter()
	filter := newNativeToolStreamFilter(Anthropic, renames)
	nativeFilterNone(t, filter, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"shell","id":"c1","input":{}}}`)
	builtin := nativeFilterOne(t, filter, `{"type":"content_block_start","index":2,"content_block":{"type":"server_tool_use","name":"web_search","id":"s1","input":{"query":"q"}}}`)
	if builtin["content_block"].(map[string]any)["name"] != "web_search" {
		t.Fatalf("builtin changed: %v", builtin)
	}
	nativeFilterOne(t, filter, `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)
	nativeFilterOne(t, filter, `{"type":"content_block_stop","index":2}`)
	stop := nativeFilterOne(t, filter, `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`)
	if stop["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("tool stop with retained server builtin was demoted: %v", stop)
	}

	filter = newNativeToolStreamFilter(Anthropic, renames)
	nativeFilterNone(t, filter, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"shell","id":"c1","input":{}}}`)
	stop = nativeFilterOne(t, filter, `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`)
	if stop["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("tool stop without any retained tool not demoted: %v", stop)
	}

	filter = newNativeToolStreamFilter(Anthropic, renames)
	nativeFilterOne(t, filter, `{"type":"message_start","message":{"content":[{"type":"server_tool_use","name":"web_search","id":"s2","input":{}}]}}`)
	stop = nativeFilterOne(t, filter, `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`)
	if stop["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("embedded server builtin did not retain tool stop: %v", stop)
	}
}

func TestNativeAnthropicEmbeddedMessageFiltered(t *testing.T) {
	filter := newNativeToolStreamFilter(Anthropic, nativeFilterRenames())
	out := nativeFilterOne(t, filter, `{"type":"message_start","message":{"id":"m","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"bad","name":"unknown","input":{}},{"type":"tool_use","id":"ok","name":"shell","input":{"command":"pwd"}}],"stop_reason":"tool_use"}}`)
	message := out["message"].(map[string]any)
	content := message["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["name"] != "exec_command" || content[1].(map[string]any)["input"].(map[string]any)["cmd"] != "pwd" {
		t.Fatalf("embedded message content not filtered/mapped: %v", message)
	}
}

func TestNativeResponsesMixedItemsAndRelatedEvents(t *testing.T) {
	filter := newNativeToolStreamFilter(Responses, nativeFilterRenames())
	nativeFilterNone(t, filter, `{"type":"response.output_item.added","output_index":0,"item":{"id":"bad","type":"function_call","call_id":"badcall","name":"unknown","arguments":""}}`)
	for _, raw := range []string{
		`{"type":"response.function_call_arguments.delta","item_id":"bad","delta":"secret"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"secret"}`,
		`{"type":"response.item_status.changed","item_id":"bad","status":"completed"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"bad","type":"function_call","call_id":"badcall","name":"unknown","arguments":"{}"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"bad","type":"function_call","call_id":"badcall","name":"clnt_read","arguments":"{}"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"unseen","delta":"secret"}`,
		`{"type":"response.function_call_arguments.done","output_index":99,"arguments":"secret"}`,
	} {
		nativeFilterNone(t, filter, raw)
	}
	start := nativeFilterOne(t, filter, `{"type":"response.output_item.added","output_index":5,"item":{"id":"good","type":"function_call","call_id":"call5","name":"clnt_read","arguments":"","status":"in_progress"}}`)
	if item := start["item"].(map[string]any); item["name"] != "read_file" || item["call_id"] != "call5" || item["id"] != "good" || start["output_index"] != float64(5) {
		t.Fatalf("allowed item changed identity: %v", start)
	}
	nativeFilterOne(t, filter, `{"type":"response.function_call_arguments.delta","item_id":"good","delta":"{\"path\":\"a\"}"}`)
	nativeFilterOne(t, filter, `{"type":"response.function_call_arguments.done","output_index":5,"arguments":"{\"path\":\"a\"}"}`)
	done := nativeFilterOne(t, filter, `{"type":"response.output_item.done","item":{"id":"good","type":"function_call","name":"clnt_read","call_id":"call5","arguments":"{\"path\":\"a\"}"}}`)
	if done["item"].(map[string]any)["name"] != "read_file" {
		t.Fatalf("done item name not restored: %v", done)
	}
	for _, raw := range []string{
		`{"type":"response.output_item.added","output_index":6,"item":{"id":"msg","type":"message","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":6,"item_id":"msg","delta":"hello"}`,
		`{"type":"response.output_text.done","output_index":6,"item_id":"msg","text":"hello"}`,
		`{"type":"response.output_item.added","output_index":7,"item":{"id":"web","type":"web_search_call","status":"in_progress"}}`,
		`{"type":"response.web_search_call.completed","output_index":7,"item_id":"web"}`,
		`{"type":"error","item_id":"bad","error":{"message":"broken"}}`,
		`{"type":"response.completed","item_id":"bad","response":{"output":[],"usage":{"total_tokens":3}}}`,
	} {
		want := nativeFilterPayload(t, raw)
		got := filter.rewrite(want)
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("text/builtin/error changed: %s => %v", raw, got)
		}
	}
}

func TestNativeResponsesOfficialArgumentsBufferAndMap(t *testing.T) {
	filter := newNativeToolStreamFilter(Responses, nativeFilterRenames())
	nativeFilterOne(t, filter, `{"type":"response.output_item.added","output_index":2,"item":{"id":"fc2","type":"function_call","name":"shell","call_id":"call2","arguments":""}}`)
	nativeFilterNone(t, filter, `{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc2","delta":"{\"command\":","extra":"keep"}`)
	nativeFilterNone(t, filter, `{"type":"response.function_call_arguments.delta","item_id":"fc2","delta":"\"pwd\",\"timeout\":2}"}`)
	out := filter.rewrite(nativeFilterPayload(t, `{"type":"response.function_call_arguments.done","item_id":"fc2","output_index":2,"arguments":"{\"command\":\"pwd\",\"timeout\":2}"}`))
	if len(out) != 2 || out[0]["type"] != "response.function_call_arguments.delta" || out[0]["item_id"] != "fc2" || out[0]["output_index"] != float64(2) || out[0]["extra"] != "keep" || out[0]["delta"] != `{"cmd":"pwd"}` || out[1]["arguments"] != `{"cmd":"pwd"}` {
		t.Fatalf("mapped delta/done mismatch: %v", out)
	}
	done := nativeFilterOne(t, filter, `{"type":"response.output_item.done","output_index":2,"item":{"id":"fc2","type":"function_call","name":"shell","call_id":"call2","arguments":"{\"command\":\"pwd\",\"timeout\":2}"}}`)
	item := done["item"].(map[string]any)
	if item["name"] != "exec_command" || item["arguments"] != `{"cmd":"pwd"}` || item["call_id"] != "call2" {
		t.Fatalf("done item mapping mismatch: %v", done)
	}
}

func TestNativeResponsesFlushesArgumentsBeforeItemDone(t *testing.T) {
	for _, test := range []struct {
		name      string
		fragments string
		doneArgs  string
		want      string
	}{
		{name: "complete item arguments win", fragments: `{"command":"partial`, doneArgs: `{"command":"from done","timeout":5}`, want: `{"cmd":"from done"}`},
		{name: "missing item arguments uses fragments", fragments: `{"command":"from deltas","timeout":5}`, want: `{"cmd":"from deltas"}`},
		{name: "invalid item arguments uses fragments", fragments: `{"command":"from deltas","timeout":5}`, doneArgs: `{`, want: `{"cmd":"from deltas"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			filter := newNativeToolStreamFilter(Responses, nativeFilterRenames())
			nativeFilterOne(t, filter, `{"type":"response.output_item.added","output_index":2,"item":{"id":"fc2","type":"function_call","name":"shell","call_id":"call2","arguments":""}}`)
			for _, fragment := range []string{test.fragments[:5], test.fragments[5:]} {
				if out := filter.rewrite(map[string]any{"type": "response.function_call_arguments.delta", "output_index": float64(2), "item_id": "fc2", "delta": fragment, "extra": "preserve"}); len(out) != 0 {
					t.Fatalf("official argument delta escaped buffer: %v", out)
				}
			}
			payload := nativeFilterPayload(t, `{"type":"response.output_item.done","output_index":2,"item":{"id":"fc2","type":"function_call","name":"shell","call_id":"call2"}}`)
			if test.doneArgs != "" {
				payload["item"].(map[string]any)["arguments"] = test.doneArgs
			}
			out := filter.rewrite(payload)
			if len(out) != 2 || out[0]["type"] != "response.function_call_arguments.delta" || out[0]["delta"] != test.want || out[0]["item_id"] != "fc2" || out[0]["output_index"] != float64(2) || out[0]["extra"] != "preserve" || out[1]["type"] != "response.output_item.done" {
				t.Fatalf("missing arguments.done did not flush before item.done: %v", out)
			}
			item := out[1]["item"].(map[string]any)
			if item["name"] != "exec_command" || item["arguments"] != test.want || item["id"] != "fc2" || item["call_id"] != "call2" {
				t.Fatalf("done item lost mapped arguments or identity: %v", item)
			}
			if repeated := filter.rewrite(payload); len(repeated) != 1 {
				t.Fatalf("repeated item.done duplicated argument flush: %v", repeated)
			}
		})
	}
}

func TestNativeResponsesFinalOutputsFilteredAndMapped(t *testing.T) {
	filter := newNativeToolStreamFilter(Responses, nativeFilterRenames())
	for _, kind := range []string{"response.completed", "response.incomplete", "response.failed"} {
		payload := nativeFilterPayload(t, `{"response":{"id":"r1","output":[{"id":"msg","type":"message","content":[{"type":"output_text","text":"hello"}]},{"id":"bad","type":"function_call","name":"unknown","arguments":"{}"},{"id":"ok","type":"function_call","call_id":"call1","name":"shell","arguments":"{\"command\":\"pwd\",\"timeout\":1}"},{"id":"web","type":"web_search_call"}],"usage":{"total_tokens":12},"error":{"message":"upstream"}}}`)
		payload["type"] = kind
		out := filter.rewrite(payload)
		if len(out) != 1 {
			t.Fatalf("%s removed terminal event: %v", kind, out)
		}
		response := out[0]["response"].(map[string]any)
		output := response["output"].([]any)
		if len(output) != 3 || output[1].(map[string]any)["name"] != "exec_command" || output[1].(map[string]any)["arguments"] != `{"cmd":"pwd"}` || response["usage"].(map[string]any)["total_tokens"] != float64(12) || response["error"].(map[string]any)["message"] != "upstream" {
			t.Fatalf("%s final response mismatch: %v", kind, response)
		}
	}
}

func TestNativeResponsesNoToolsCustomCallsAndAliasArguments(t *testing.T) {
	renames := NewToolRenames()
	renames.EnableResponseFilter()
	filter := newNativeToolStreamFilter(Responses, renames)
	nativeFilterNone(t, filter, `{"type":"response.output_item.added","output_index":0,"item":{"id":"custom","type":"custom_tool_call","name":"unknown","input":"raw"}}`)
	nativeFilterNone(t, filter, `{"type":"response.custom_tool_call_input.delta","item_id":"custom","delta":"secret"}`)
	nativeFilterNone(t, filter, `{"type":"response.custom_tool_call_input.done","item_id":"custom","input":"secret"}`)
	nativeFilterOne(t, filter, `{"type":"response.output_text.delta","delta":"no tools"}`)

	renames = nativeFilterRenames()
	// A wire alias carries client-schema arguments; a coincidental mapping for
	// the alias must not make it buffer or rewrite those arguments.
	renames.BindOfficialArgs("clnt_exec", map[string]string{"cmd": "wrong"})
	filter = newNativeToolStreamFilter(Responses, renames)
	nativeFilterOne(t, filter, `{"type":"response.output_item.added","output_index":1,"item":{"id":"alias","type":"function_call","name":"clnt_exec","arguments":""}}`)
	got := nativeFilterOne(t, filter, `{"type":"response.function_call_arguments.delta","item_id":"alias","delta":"{\"cmd\":\"pwd\"}"}`)
	if got["delta"] != `{"cmd":"pwd"}` {
		t.Fatalf("alias arguments remapped: %v", got)
	}
	custom := nativeFilterOne(t, filter, `{"type":"response.output_item.added","output_index":2,"item":{"id":"custom","type":"custom_tool_call","name":"clnt_read","call_id":"customcall","input":"raw"}}`)
	if custom["item"].(map[string]any)["name"] != "read_file" {
		t.Fatalf("custom tool name not restored: %v", custom)
	}
	nativeFilterOne(t, filter, `{"type":"response.custom_tool_call_input.delta","item_id":"custom","delta":"raw"}`)
	nativeFilterOne(t, filter, `{"type":"response.custom_tool_call_input.done","item_id":"custom","input":"raw"}`)
}
