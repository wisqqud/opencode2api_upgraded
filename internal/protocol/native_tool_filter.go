package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// nativeToolStreamFilter bounds native SSE tool events to the tools declared by
// the client. It works on upstream objects, before any stateless name rewrite,
// because official argument schemas are selected by the original tool name.
type nativeToolStreamFilter struct {
	protocol Protocol
	renames  *ToolRenames
	blocks   map[string]*nativeToolFilterState
	outputs  map[string]*nativeToolFilterState
	items    map[string]*nativeToolFilterState
	keptTool bool
}

type nativeToolFilterState struct {
	allowed bool
	tool    bool
	mapping map[string]string
	args    strings.Builder
	delta   map[string]any
	flushed bool
}

// newNativeToolStreamFilter returns nil for rename-only requests. An enabled
// filter with no declared tools still needs state, to suppress all tool calls.
func newNativeToolStreamFilter(protocol Protocol, renames *ToolRenames) *nativeToolStreamFilter {
	if !renames.RestrictsResponses() || (protocol != Anthropic && protocol != Responses) {
		return nil
	}
	return &nativeToolStreamFilter{
		protocol: protocol,
		renames:  renames,
		blocks:   map[string]*nativeToolFilterState{},
		outputs:  map[string]*nativeToolFilterState{},
		items:    map[string]*nativeToolFilterState{},
	}
}

// rewrite returns zero, one or several native events. Argument fragments that
// need schema conversion are withheld until a complete JSON object is available.
func (filter *nativeToolStreamFilter) rewrite(payload map[string]any) []map[string]any {
	if filter == nil {
		return []map[string]any{payload}
	}
	if filter.protocol == Anthropic {
		return filter.rewriteAnthropic(payload)
	}
	return filter.rewriteResponses(payload)
}

func (filter *nativeToolStreamFilter) rewriteAnthropic(payload map[string]any) []map[string]any {
	kind, _ := payload["type"].(string)
	index := nativeToolIndex(payload, "index")
	switch kind {
	case "content_block_start":
		block, _ := payload["content_block"].(map[string]any)
		blockType, _ := block["type"].(string)
		state := &nativeToolFilterState{allowed: true}
		if blockType == "tool_use" {
			name, _ := block["name"].(string)
			state.tool = true
			state.allowed = filter.renames.AllowsResponseTool(name)
			state.mapping = filter.argumentMapping(name)
			if state.allowed {
				filter.keptTool = true
			}
		} else if blockType == "server_tool_use" {
			filter.keptTool = true
		}
		filter.blocks[index] = state
		if !state.allowed {
			return nil
		}
	case "content_block_delta":
		state := filter.blocks[index]
		if state != nil && !state.allowed {
			return nil
		}
		delta, _ := payload["delta"].(map[string]any)
		if delta["type"] == "input_json_delta" && state != nil && len(state.mapping) > 0 {
			part, _ := delta["partial_json"].(string)
			state.args.WriteString(part)
			if state.delta == nil {
				state.delta = nativeToolCopy(payload)
			}
			return nil
		}
	case "content_block_stop":
		state := filter.blocks[index]
		if state != nil && !state.allowed {
			return nil
		}
		if state != nil && state.delta != nil && !state.flushed {
			state.flushed = true
			delta := nativeToolCopy(state.delta)
			body, _ := state.delta["delta"].(map[string]any)
			body = nativeToolCopy(body)
			body["partial_json"] = RemapArguments(state.args.String(), state.mapping)
			delta["delta"] = body
			return []map[string]any{delta, payload}
		}
	case "message_delta":
		delta, _ := payload["delta"].(map[string]any)
		if delta["stop_reason"] == "tool_use" && !filter.keptTool {
			delta["stop_reason"] = "end_turn"
		}
	}
	payload = filter.renameComplete(payload)
	// Anthropic's message_start embeds the message rather than exposing its
	// content at the event root. Apply the same non-stream filter to that body.
	if message, ok := payload["message"].(map[string]any); ok {
		message = filter.renameComplete(message)
		payload["message"] = message
		filter.noteAnthropicTools(message)
	}
	filter.noteAnthropicTools(payload)
	return []map[string]any{payload}
}

func (filter *nativeToolStreamFilter) noteAnthropicTools(message map[string]any) {
	content, _ := message["content"].([]any)
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if block["type"] == "tool_use" || block["type"] == "server_tool_use" {
			filter.keptTool = true
		}
	}
}

func (filter *nativeToolStreamFilter) rewriteResponses(payload map[string]any) []map[string]any {
	kind, _ := payload["type"].(string)
	if kind == "response.output_item.added" || kind == "response.output_item.done" {
		item, _ := payload["item"].(map[string]any)
		itemType, _ := item["type"].(string)
		state := filter.responseState(payload)
		if state != nil && !state.allowed {
			// Once its start was removed, later item events must not introduce
			// the same call halfway through the stream under a different name.
			filter.bindResponseState(payload, state)
			return nil
		}
		if state == nil {
			state = &nativeToolFilterState{allowed: true}
		}
		if itemType == "function_call" || itemType == "custom_tool_call" {
			name, _ := item["name"].(string)
			if name != "" || !state.tool {
				state.allowed = filter.renames.AllowsResponseTool(name)
				if itemType == "function_call" && (!state.tool || state.mapping == nil) {
					state.mapping = filter.argumentMapping(name)
				}
			}
			state.tool = true
		}
		filter.bindResponseState(payload, state)
		if !state.allowed {
			return nil
		}
		if kind == "response.output_item.done" && len(state.mapping) > 0 && state.delta != nil && !state.flushed {
			// Some upstreams omit arguments.done. Flush before the item closes,
			// preferring its complete arguments over potentially partial deltas.
			raw, _ := item["arguments"].(string)
			if raw == "" || !json.Valid([]byte(raw)) {
				raw = state.args.String()
			}
			mapped := RemapArguments(raw, state.mapping)
			state.flushed = true
			delta := nativeToolCopy(state.delta)
			delta["delta"] = mapped
			done := filter.renameComplete(payload)
			if doneItem, ok := done["item"].(map[string]any); ok {
				doneItem["arguments"] = mapped
			}
			return []map[string]any{delta, done}
		}
		return []map[string]any{filter.renameComplete(payload)}
	}

	state := filter.responseState(payload)
	// Errors and response lifecycle events must survive even when they carry an
	// item identifier for context. Their final output arrays are filtered below.
	switch kind {
	case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed", "response.queued", "response.cancelled":
		return []map[string]any{filter.renameComplete(payload)}
	}
	if kind == "error" || strings.HasSuffix(kind, ".error") {
		return []map[string]any{filter.renameComplete(payload)}
	}
	arguments := strings.HasPrefix(kind, "response.function_call_arguments.") ||
		strings.HasPrefix(kind, "response.custom_tool_call_input.")
	if arguments {
		// An argument event without a known executable item has no trustworthy
		// tool identity; emitting it could leak a tool removed by a final output.
		if state == nil || !state.allowed || !state.tool {
			return nil
		}
		if len(state.mapping) > 0 && strings.HasPrefix(kind, "response.function_call_arguments.") {
			switch {
			case strings.HasSuffix(kind, ".delta"):
				part, _ := payload["delta"].(string)
				state.args.WriteString(part)
				if state.delta == nil {
					state.delta = nativeToolCopy(payload)
				}
				return nil
			case strings.HasSuffix(kind, ".done"):
				raw, _ := payload["arguments"].(string)
				if raw == "" {
					raw = state.args.String()
				}
				mapped := RemapArguments(raw, state.mapping)
				if _, exists := payload["arguments"]; exists || raw != "" {
					payload["arguments"] = mapped
				}
				if !state.flushed && (state.delta != nil || raw != "") {
					state.flushed = true
					delta := nativeToolCopy(state.delta)
					if state.delta == nil {
						delta = nativeToolCopy(payload)
						delete(delta, "arguments")
					}
					delta["type"] = "response.function_call_arguments.delta"
					delta["delta"] = mapped
					return []map[string]any{delta, payload}
				}
			}
		}
	}
	if state != nil && !state.allowed {
		return nil
	}
	return []map[string]any{filter.renameComplete(payload)}
}

// argumentMapping deliberately uses the raw name. Client wire aliases already
// use the client's schema, even if a mapping was accidentally bound to one.
func (filter *nativeToolStreamFilter) argumentMapping(rawName string) map[string]string {
	if client, alias := filter.renames.Client(rawName); alias && client != rawName {
		return nil
	}
	mapping, _ := filter.renames.OfficialArgs(rawName)
	return mapping
}

func (filter *nativeToolStreamFilter) responseState(payload map[string]any) *nativeToolFilterState {
	if id, _ := payload["item_id"].(string); id != "" {
		if state := filter.items[id]; state != nil {
			return state
		}
	}
	if item, ok := payload["item"].(map[string]any); ok {
		if id, _ := item["id"].(string); id != "" {
			if state := filter.items[id]; state != nil {
				return state
			}
		}
	}
	return filter.outputs[nativeToolIndex(payload, "output_index")]
}

func (filter *nativeToolStreamFilter) bindResponseState(payload map[string]any, state *nativeToolFilterState) {
	if index := nativeToolIndex(payload, "output_index"); index != "" {
		filter.outputs[index] = state
	}
	if id, _ := payload["item_id"].(string); id != "" {
		filter.items[id] = state
	}
	if item, ok := payload["item"].(map[string]any); ok {
		if id, _ := item["id"].(string); id != "" {
			filter.items[id] = state
		}
	}
}

func (filter *nativeToolStreamFilter) renameComplete(payload map[string]any) map[string]any {
	body, err := json.Marshal(payload)
	if err != nil {
		return payload
	}
	body = RenameResponseTool(body, filter.protocol, filter.renames)
	var rewritten map[string]any
	if json.Unmarshal(body, &rewritten) != nil {
		return payload
	}
	return rewritten
}

func nativeToolIndex(payload map[string]any, key string) string {
	value, exists := payload[key]
	if !exists || value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func nativeToolCopy(payload map[string]any) map[string]any {
	copy := make(map[string]any, len(payload))
	for key, value := range payload {
		copy[key] = value
	}
	return copy
}
