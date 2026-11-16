package protocol

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func filteredStream(t *testing.T, p Protocol, r *ToolRenames, frames ...string) string {
	t.Helper()
	raw := strings.Join(frames, "\n\n") + "\n\n"
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(raw), p, r))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
func TestMappedStreamNameEmittedOnce(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		for _, terminal := range []string{`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, `data: [DONE]`} {
			r := NewToolRenames()
			r.Add("edit", ClientToolWireName("edit", nil))
			r.BindOfficial("edit", "edit")
			r.BindOfficialArgs("edit", map[string]string{"path": "file_path"})
			if filtered {
				r.EnableResponseFilter()
			}
			out := filteredStream(t, Chat, r, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_edit","function":{"name":"edit","arguments":"{\"path\":\"a\"}"}}]}}]}`, terminal)
			if strings.Count(out, `"name":"edit"`) != 1 || !strings.Contains(out, `\"file_path\"`) {
				t.Fatalf("filtered=%v terminal=%s: %s", filtered, terminal, out)
			}
		}
	}
}

func TestFilteredChatStreamMixedCalls(t *testing.T) {
	r := NewToolRenames()
	r.EnableResponseFilter()
	r.Add("edit", ClientToolWireName("edit", nil))
	r.BindOfficial("edit", "edit")
	r.BindOfficialArgs("edit", map[string]string{"path": "file_path"})
	alias, _ := r.Wire("edit")
	out := filteredStream(t, Chat, r,
		`data: {"choices":[{"index":0,"delta":{"content":"hello","tool_calls":[{"index":0,"id":"bad","type":"function","function":{"name":"question","arguments":"{"}},{"index":1,"id":"good","type":"function","function":{"name":"edit","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}},{"index":1,"function":{"arguments":"th\":\"/tmp/a\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"alias","function":{"name":"`+alias[:9]+`","arguments":"{\"custom\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"function":{"name":"`+alias[9:]+`","arguments":"true}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"total_tokens":42}}`, `data: [DONE]`)
	if strings.Contains(out, "question") || strings.Contains(out, `"id":"bad"`) || strings.Contains(out, alias) {
		t.Fatal(out)
	}
	if !strings.Contains(out, `"id":"good"`) || !strings.Contains(out, `"id":"alias"`) || !strings.Contains(out, `\"file_path\"`) || !strings.Contains(out, `"finish_reason":"tool_calls"`) || !strings.Contains(out, `"total_tokens":42`) {
		t.Fatal(out)
	}
	if strings.Contains(out, `\"path\"`) {
		t.Fatal("same-name official arguments not mapped: " + out)
	}
}
func TestFilteredChatStreamAllDroppedAndChoices(t *testing.T) {
	r := NewToolRenames()
	r.EnableResponseFilter()
	r.Add("read", ClientToolWireName("read", nil))
	r.BindOfficial("read", "read")
	out := filteredStream(t, Chat, r,
		`data: {"choices":[{"index":0,"delta":{"content":"kept","tool_calls":[{"index":0,"id":"drop","function":{"name":"invented","arguments":"{}"}}]}},{"index":1,"delta":{"tool_calls":[{"index":0,"id":"keep","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"},{"index":1,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	if strings.Contains(out, "invented") || strings.Contains(out, `"id":"drop"`) || !strings.Contains(out, `"id":"keep"`) {
		t.Fatal(out)
	}
	if !strings.Contains(out, `"finish_reason":"stop","index":0`) || !strings.Contains(out, `"finish_reason":"tool_calls","index":1`) {
		t.Fatal(out)
	}
}
func TestFilteredStreamNoToolsAndLegacy(t *testing.T) {
	r := NewToolRenames()
	r.EnableResponseFilter()
	out := filteredStream(t, Chat, r, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"bad","function":{"name":"edit","arguments":"{}"}}]}}]}`, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, `data: [DONE]`)
	if strings.Contains(out, "tool_calls") || strings.Contains(out, "bad") || !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatal(out)
	}
	r.Add("Read", ClientToolWireName("Read", nil))
	r.BindOfficial("read", "Read")
	out = filteredStream(t, Chat, r, `data: {"choices":[{"index":0,"delta":{"function_call":{"name":"read","arguments":"{"}}}]}`, `data: {"choices":[{"index":0,"delta":{"function_call":{"arguments":"}"}}}]}`, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"function_call"}]}`, `data: [DONE]`)
	if !strings.Contains(out, `"name":"Read"`) || !strings.Contains(out, `"arguments":"{}"`) || !strings.Contains(out, `"finish_reason":"function_call"`) {
		t.Fatal(out)
	}
}
func TestFilteredStreamTruncation(t *testing.T) {
	r := NewToolRenames()
	r.EnableResponseFilter()
	raw := `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(raw), Chat, r))
	if err == nil || len(out) != 0 {
		t.Fatalf("truncated record became successful output: %q err=%v", out, err)
	}
}

func TestFilteredNativeSSERecords(t *testing.T) {
	r := NewToolRenames()
	r.EnableResponseFilter()
	r.Add("Edit", ClientToolWireName("Edit", nil))
	r.BindOfficial("edit", "Edit")
	r.BindOfficialArgs("edit", map[string]string{"path": "file_path"})
	out := filteredStream(t, Anthropic, r,
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"bad\",\"name\":\"question\",\"input\":{}}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"good","name":"edit","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/a\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`, `data: {"type":"message_stop"}`)
	if strings.Contains(out, "question") || strings.Contains(out, `"index":0`) || strings.Contains(out, "bad") {
		t.Fatal(out)
	}
	if !strings.Contains(out, `\"file_path\"`) || !strings.Contains(out, `"name":"Edit"`) {
		t.Fatal(out)
	}
	events := 0
	if err := readSSE(strings.NewReader(out), func(event, data string) error {
		var v map[string]any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return err
		}
		if event != v["type"] {
			t.Fatalf("orphan event header %s %s", event, data)
		}
		events++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if events != 5 {
		t.Fatalf("events=%d: %s", events, out)
	}
	out = filteredStream(t, Responses, r,
		`event: response.output_item.added`+"\n"+`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"bad","name":"question"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"bad","delta":"{}"}`,
		`data: {"type":"response.completed","response":{"output":[{"type":"function_call","id":"bad","name":"question"},{"type":"message","content":[]}],"usage":{"total_tokens":9}}}`)
	if strings.Contains(out, "bad") || strings.Contains(out, "question") || strings.Contains(out, "event: response.output_item.added") || !strings.Contains(out, "total_tokens") {
		t.Fatal(out)
	}
}
