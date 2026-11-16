package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"opencode2api/internal/jsonutil"
)

// toolBridgeReader holds tool metadata and argument fragments until a complete
// call can be adapted. Text/reasoning continue streaming. Invalid calls end the
// reader with an error before that call or a normal terminal event is emitted.
type toolBridgeReader struct {
	input    *bufio.Reader
	protocol Protocol
	renames  *ToolRenames
	pending  []byte
	err      error
	calls    map[string]map[string]any
	order    []string
	bytes    int
}

func newToolBridgeReader(input io.Reader, protocol Protocol, r *ToolRenames) io.Reader {
	if !r.HasShellBridge() {
		return input
	}
	return &toolBridgeReader{input: bufio.NewReader(input), protocol: protocol, renames: r, calls: map[string]map[string]any{}}
}

func (r *toolBridgeReader) Read(dst []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		var record strings.Builder
		for {
			line, err := r.input.ReadString('\n')
			record.WriteString(line)
			if record.Len() > 8<<20 {
				r.err = fmt.Errorf("tool bridge SSE record exceeds size limit")
				break
			}
			if err != nil {
				r.err = err
				break
			}
			if strings.TrimRight(line, "\r\n") == "" {
				break
			}
		}
		if r.err != nil {
			if record.Len() > 0 || len(r.calls) > 0 {
				r.err = errSSEUnexpectedEOF
			}
			continue
		}
		var data []string
		for _, line := range strings.Split(record.String(), "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(line[5:]))
			}
		}
		raw := strings.Join(data, "\n")
		if raw == "" {
			r.pending = []byte(record.String())
			continue
		}
		if raw == "[DONE]" {
			if len(r.calls) > 0 {
				r.err = errSSEUnexpectedEOF
				continue
			}
			r.pending = []byte(record.String())
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			r.err = err
			continue
		}
		var events []map[string]any
		var err error
		switch r.protocol {
		case Chat:
			events, err = r.chat(payload)
		case Anthropic:
			events, err = r.anthropic(payload)
		case Responses:
			events, err = r.responses(payload)
		default:
			events = []map[string]any{payload}
		}
		if err != nil {
			r.err = err
			continue
		}
		for _, event := range events {
			b, err := json.Marshal(event)
			if err != nil {
				r.err = err
				break
			}
			if r.protocol != Chat {
				if typ := jsonutil.StringAt(event, "type"); typ != "" {
					r.pending = append(r.pending, []byte("event: "+typ+"\n")...)
				}
			}
			r.pending = append(r.pending, []byte("data: ")...)
			r.pending = append(r.pending, b...)
			r.pending = append(r.pending, '\n', '\n')
		}
	}
	n := copy(dst, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *toolBridgeReader) hold(key string, call map[string]any) error {
	if _, ok := r.calls[key]; !ok {
		r.order = append(r.order, key)
	}
	r.calls[key] = call
	if len(r.calls) > 1024 {
		return fmt.Errorf("tool bridge call count exceeds limit")
	}
	return nil
}

func (r *toolBridgeReader) addBytes(n int) error {
	r.bytes += n
	if r.bytes > 8<<20 {
		return fmt.Errorf("tool bridge arguments exceed size limit")
	}
	return nil
}

func (r *toolBridgeReader) chat(payload map[string]any) ([]map[string]any, error) {
	for pos, raw := range jsonutil.SliceAt(payload, "choices") {
		choice, _ := raw.(map[string]any)
		// Some upstreams put a complete message in an SSE record. Normalize
		// it before buffering so downstream stream parsers retain its text.
		if message := jsonutil.MapAt(choice, "message"); message != nil && jsonutil.MapAt(choice, "delta") == nil {
			choice["delta"] = message
			delete(choice, "message")
			payload["object"] = "chat.completion.chunk"
		}
		prefix := fmt.Sprint(jsonutil.FirstAny(choice["index"], pos)) + ":"
		for _, field := range []string{"delta", "message"} {
			message := jsonutil.MapAt(choice, field)
			for i, rawCall := range jsonutil.SliceAt(message, "tool_calls") {
				call, _ := rawCall.(map[string]any)
				key := prefix + fmt.Sprint(jsonutil.FirstAny(call["index"], i))
				if err := r.addBytes(len(jsonutil.StringAt(call, "function", "arguments"))); err != nil {
					return nil, err
				}
				if err := r.hold(key, mergePendingChatCall(r.calls[key], call)); err != nil {
					return nil, err
				}
			}
			delete(message, "tool_calls")
			if fn := jsonutil.MapAt(message, "function_call"); fn != nil {
				if err := r.addBytes(len(jsonutil.StringAt(fn, "arguments"))); err != nil {
					return nil, err
				}
				if err := r.hold(prefix+"legacy", mergePendingChatCall(r.calls[prefix+"legacy"], map[string]any{"function": fn})); err != nil {
					return nil, err
				}
				delete(message, "function_call")
			}
		}
		if reason := jsonutil.StringAt(choice, "finish_reason"); reason != "" {
			var completed []any
			for _, key := range r.order {
				call := r.calls[key]
				if call == nil || !strings.HasPrefix(key, prefix) {
					continue
				}
				if reason != "tool_calls" && reason != "function_call" {
					return nil, toolBridgeError(jsonutil.StringAt(call, "function", "name"), "tool arguments ended without tool completion")
				}
				if err := r.renames.adaptToolCall(jsonutil.MapAt(call, "function"), false); err != nil {
					return nil, err
				}
				completed = append(completed, call)
			}
			if len(completed) > 0 {
				message := jsonutil.MapAt(choice, "delta")
				if message == nil {
					message = jsonutil.MapAt(choice, "message")
				}
				if message == nil {
					message = map[string]any{}
					choice["delta"] = message
				}
				if reason == "function_call" {
					message["function_call"] = jsonutil.MapAt(completed[0].(map[string]any), "function")
				} else {
					message["tool_calls"] = completed
				}
				for _, key := range r.order {
					if strings.HasPrefix(key, prefix) {
						delete(r.calls, key)
					}
				}
			}
		}
	}
	return []map[string]any{payload}, nil
}

func (r *toolBridgeReader) anthropic(payload map[string]any) ([]map[string]any, error) {
	key := fmt.Sprint(payload["index"])
	switch jsonutil.StringAt(payload, "type") {
	case "content_block_start":
		block := jsonutil.MapAt(payload, "content_block")
		if jsonutil.StringAt(block, "type") != "tool_use" {
			break
		}
		if !r.renames.CanExecuteOfficial(jsonutil.StringAt(block, "name")) {
			return nil, toolBridgeError(jsonutil.StringAt(block, "name"), "no executable client tool")
		}
		if err := r.hold(key, payload); err != nil {
			return nil, err
		}
		return nil, nil
	case "content_block_delta":
		if start := r.calls[key]; start != nil {
			part := jsonutil.StringAt(payload, "delta", "partial_json")
			if err := r.addBytes(len(part)); err != nil {
				return nil, err
			}
			start["bridge_args"] = jsonutil.StringAt(start, "bridge_args") + part
			return nil, nil
		}
	case "content_block_stop":
		if start := r.calls[key]; start != nil {
			block := jsonutil.MapAt(start, "content_block")
			if args := jsonutil.StringAt(start, "bridge_args"); args != "" {
				var input map[string]any
				if err := json.Unmarshal([]byte(args), &input); err != nil {
					return nil, err
				}
				block["input"] = input
			}
			if err := r.renames.adaptToolCall(block, true); err != nil {
				return nil, err
			}
			b, _ := json.Marshal(block["input"])
			block["input"] = map[string]any{}
			delete(start, "bridge_args")
			delete(r.calls, key)
			delta := map[string]any{"type": "content_block_delta", "index": payload["index"], "delta": map[string]any{"type": "input_json_delta", "partial_json": string(b)}}
			return []map[string]any{start, delta, payload}, nil
		}
	case "message_delta", "message_stop":
		if len(r.calls) > 0 {
			return nil, errSSEUnexpectedEOF
		}
	case "error":
		r.calls = map[string]map[string]any{}
	}
	return []map[string]any{payload}, nil
}

func (r *toolBridgeReader) responses(payload map[string]any) ([]map[string]any, error) {
	kind := jsonutil.StringAt(payload, "type")
	item := jsonutil.MapAt(payload, "item")
	key := jsonutil.FirstString(jsonutil.StringAt(payload, "item_id"), jsonutil.StringAt(item, "id"))
	switch kind {
	case "response.output_item.added":
		typ := jsonutil.StringAt(item, "type")
		if typ != "function_call" && typ != "custom_tool_call" && typ != "tool_call" {
			break
		}
		if !r.renames.CanExecuteOfficial(jsonutil.StringAt(item, "name")) {
			return nil, toolBridgeError(jsonutil.StringAt(item, "name"), "no executable client tool")
		}
		if err := r.hold(key, payload); err != nil {
			return nil, err
		}
		return nil, nil
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if start := r.calls[key]; start != nil {
			part := jsonutil.StringAt(payload, "delta")
			if err := r.addBytes(len(part)); err != nil {
				return nil, err
			}
			start["bridge_args"] = jsonutil.StringAt(start, "bridge_args") + part
			return nil, nil
		}
		return nil, toolBridgeError(key, "argument delta without tool metadata")
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		if start := r.calls[key]; start != nil {
			field := "arguments"
			if strings.HasPrefix(kind, "response.custom_tool_call_input.") {
				field = "input"
			}
			start["bridge_args"] = payload[field]
			return nil, nil
		}
		return nil, toolBridgeError(key, "argument completion without tool metadata")
	case "response.output_item.done":
		if start := r.calls[key]; start != nil {
			return r.finishResponseCall(key, start, item)
		}
		if typ := jsonutil.StringAt(item, "type"); typ == "function_call" || typ == "custom_tool_call" || typ == "tool_call" {
			start := map[string]any{"type": "response.output_item.added", "output_index": payload["output_index"], "item": item}
			return r.finishResponseCall(key, start, item)
		}
	case "response.completed", "response.done":
		var events []map[string]any
		response := jsonutil.MapAt(payload, "response")
		for _, raw := range jsonutil.SliceAt(response, "output") {
			call, _ := raw.(map[string]any)
			id := jsonutil.StringAt(call, "id")
			if start := r.calls[id]; start != nil {
				done, err := r.finishResponseCall(id, start, call)
				if err != nil {
					return nil, err
				}
				events = append(events, done...)
			}
		}
		if len(r.calls) > 0 {
			return nil, errSSEUnexpectedEOF
		}
		if err := adaptResponseMap(response, Responses, r.renames); err != nil {
			return nil, err
		}
		return append(events, payload), nil
	case "response.failed", "response.incomplete", "error":
		r.calls = map[string]map[string]any{}
	}
	return []map[string]any{payload}, nil
}

func (r *toolBridgeReader) finishResponseCall(key string, start, item map[string]any) ([]map[string]any, error) {
	field := "arguments"
	if jsonutil.StringAt(item, "type") == "custom_tool_call" {
		field = "input"
	}
	if jsonutil.StringAt(item, field) == "" {
		item[field] = jsonutil.StringAt(start, "bridge_args")
	}
	if err := r.renames.adaptToolCall(item, false); err != nil {
		return nil, err
	}
	delete(r.calls, key)
	delete(start, "bridge_args")
	start["item"] = item
	return []map[string]any{start, {"type": "response.output_item.done", "output_index": start["output_index"], "item": item}}, nil
}
