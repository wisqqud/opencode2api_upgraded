package protocol

import "opencode2api/internal/jsonutil"

// filterResponseToolsInMap filters complete response objects, leaving stream
// item/content_block events and Chat deltas to the stateful stream reader.
// Only client-executed calls are restricted; server-side builtins and ordinary
// content retain their original shape and order.
func filterResponseToolsInMap(payload map[string]any, protocol Protocol, renames *ToolRenames) bool {
	if !renames.RestrictsResponses() {
		return false
	}
	changed := false
	switch protocol {
	case Chat:
		for _, raw := range jsonutil.SliceAt(payload, "choices") {
			choice, _ := raw.(map[string]any)
			message, ok := choice["message"].(map[string]any)
			if !ok {
				continue
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				kept := make([]any, 0, len(calls))
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					kind := jsonutil.StringAt(call, "type")
					if kind != "" && kind != "function" {
						kept = append(kept, rawCall)
						continue
					}
					if renames.AllowsResponseTool(jsonutil.StringAt(call, "function", "name")) {
						kept = append(kept, rawCall)
					}
				}
				if len(kept) != len(calls) {
					if len(kept) == 0 {
						delete(message, "tool_calls")
					} else {
						message["tool_calls"] = kept
					}
					changed = true
				}
			}
			if function, ok := message["function_call"].(map[string]any); ok &&
				!renames.AllowsResponseTool(jsonutil.StringAt(function, "name")) {
				delete(message, "function_call")
				changed = true
			}
			_, legacy := message["function_call"].(map[string]any)
			if len(jsonutil.SliceAt(message, "tool_calls")) == 0 && !legacy {
				switch jsonutil.StringAt(choice, "finish_reason") {
				case "tool_calls", "function_call":
					choice["finish_reason"] = "stop"
					changed = true
				}
			}
		}
	case Responses:
		if filterResponsesOutput(payload, renames) {
			changed = true
		}
		if response, ok := payload["response"].(map[string]any); ok && filterResponsesOutput(response, renames) {
			changed = true
		}
	case Anthropic:
		if content, ok := payload["content"].([]any); ok {
			kept := make([]any, 0, len(content))
			keptTool := false
			for _, raw := range content {
				block, _ := raw.(map[string]any)
				switch jsonutil.StringAt(block, "type") {
				case "tool_use":
					if !renames.AllowsResponseTool(jsonutil.StringAt(block, "name")) {
						continue
					}
					keptTool = true
				case "server_tool_use":
					keptTool = true
				}
				kept = append(kept, raw)
			}
			if len(kept) != len(content) {
				payload["content"] = kept
				changed = true
			}
			if !keptTool && jsonutil.StringAt(payload, "stop_reason") == "tool_use" {
				payload["stop_reason"] = "end_turn"
				changed = true
			}
		}
	}
	return changed
}

// filterResponsesOutput deliberately ignores the SSE "item" field: removing a
// stream call requires suppressing all later events that share its identity.
func filterResponsesOutput(payload map[string]any, renames *ToolRenames) bool {
	output, ok := payload["output"].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(output))
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		switch jsonutil.StringAt(item, "type") {
		case "function_call", "custom_tool_call", "tool_call":
			if !renames.AllowsResponseTool(jsonutil.StringAt(item, "name")) {
				continue
			}
		}
		kept = append(kept, raw)
	}
	if len(kept) == len(output) {
		return false
	}
	payload["output"] = kept
	return true
}
