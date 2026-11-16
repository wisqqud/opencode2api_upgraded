package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"opencode2api/internal/jsonutil"
)

// responseToolStreamFilter runs after Chat name reconstruction. It holds the
// initial call metadata until a name is known, so a fragmented alias cannot leak
// an orphan id/argument event. Native protocols track their block/item lifetimes.
type responseToolStreamFilter struct {
	protocol Protocol
	renames  *ToolRenames
	native   *nativeToolStreamFilter
	calls    map[string]*filteredChatCall
	kept     map[string]bool
}
type filteredChatCall struct {
	decided bool
	allowed bool
	pending map[string]any
}

func newResponseToolStreamFilter(protocol Protocol, renames *ToolRenames) *responseToolStreamFilter {
	return &responseToolStreamFilter{protocol: protocol, renames: renames, native: newNativeToolStreamFilter(protocol, renames), calls: map[string]*filteredChatCall{}, kept: map[string]bool{}}
}

// readFiltered consumes complete SSE records, not individual lines: suppressing
// an event must suppress its event: header too, and multiline data is one JSON.
func (reader *renameStreamReader) readFiltered(data []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.err != nil {
			return 0, reader.err
		}
		var record strings.Builder
		complete := false
		for {
			line, err := reader.buffer.ReadString('\n')
			record.WriteString(line)
			if err != nil {
				reader.err = err
				break
			}
			if strings.TrimRight(line, "\r\n") == "" {
				complete = true
				break
			}
		}
		if record.Len() > 0 {
			if !complete {
				// Do not turn an unterminated upstream record into a complete
				// success event. Suppress its data and report stream truncation.
				reader.err = errSSEUnexpectedEOF
			} else {
				reader.pending = reader.rewriteRecord(record.String())
			}
		}
		if reader.err == io.EOF {
			reader.pending = append(reader.pending, reader.filterChatTail(reader.renamer.finish())...)
		}
	}
	n := copy(data, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

func (reader *renameStreamReader) rewriteRecord(record string) []byte {
	var lines []string
	for _, line := range strings.Split(record, "\n") {
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	raw := strings.Join(lines, "\n")
	if raw == "" {
		return []byte(record)
	}
	if raw == "[DONE]" {
		return append(reader.filterChatTail(reader.renamer.finish()), []byte(record)...)
	}
	if reader.renamer.protocol == Chat {
		raw = string(reader.renamer.rewriteFrame(raw))
	}
	var payload map[string]any
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return []byte(record)
	}
	return reader.encodeFilteredPayload(payload)
}
func (reader *renameStreamReader) encodeFilteredPayload(payload map[string]any) []byte {
	events := reader.filter.rewrite(payload)
	var out []byte
	for _, event := range events {
		b, err := json.Marshal(event)
		if err != nil {
			continue
		}
		// Native event type follows the payload when one input yields buffered
		// argument deltas followed by its original stop/done event.
		if reader.renamer.protocol != Chat {
			if typ := jsonutil.StringAt(event, "type"); typ != "" {
				out = append(out, []byte("event: "+typ+"\n")...)
			}
		}
		out = append(out, []byte("data: ")...)
		out = append(out, b...)
		out = append(out, '\n', '\n')
	}
	return out
}
func (reader *renameStreamReader) filterChatTail(tail []byte) []byte {
	if len(tail) == 0 {
		return nil
	}
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(tail)), "data:"))
	var payload map[string]any
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return nil
	}
	return reader.encodeFilteredPayload(payload)
}
func (f *responseToolStreamFilter) rewrite(payload map[string]any) []map[string]any {
	if f.protocol != Chat {
		return f.native.rewrite(payload)
	}
	for choicePosition, rawChoice := range jsonutil.SliceAt(payload, "choices") {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		choiceID := fmt.Sprint(jsonutil.FirstAny(choice["index"], choicePosition))
		for _, field := range []string{"delta", "message"} {
			message, _ := choice[field].(map[string]any)
			if message == nil {
				continue
			}
			calls, exists := message["tool_calls"].([]any)
			if exists {
				kept := make([]any, 0, len(calls))
				for position, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					if call == nil {
						continue
					}
					key := choiceID + ":" + fmt.Sprint(jsonutil.FirstAny(call["index"], jsonutil.StringAt(call, "id"), position))
					state := f.calls[key]
					if state == nil {
						state = &filteredChatCall{}
						f.calls[key] = state
					}
					fn, _ := call["function"].(map[string]any)
					if !state.decided {
						state.pending = mergePendingChatCall(state.pending, call)
						if name := jsonutil.StringAt(fn, "name"); name != "" {
							state.decided = true
							state.allowed = f.renames.AllowsResponseTool(name)
							if state.allowed {
								kept = append(kept, state.pending)
								f.kept[choiceID] = true
							}
							state.pending = nil
						}
					} else if state.allowed {
						kept = append(kept, call)
					}
				}
				if len(kept) > 0 {
					message["tool_calls"] = kept
				} else {
					delete(message, "tool_calls")
				}
			}
			if fn, ok := message["function_call"].(map[string]any); ok {
				key := choiceID + ":legacy"
				state := f.calls[key]
				if state == nil {
					state = &filteredChatCall{}
					f.calls[key] = state
				}
				// Legacy names can also be fragmented. Hold the complete function until
				// finish_reason rather than emitting an unclassified prefix.
				state.pending = mergePendingChatCall(state.pending, map[string]any{"function": fn})
				delete(message, "function_call")
			}
		}
		reason := jsonutil.StringAt(choice, "finish_reason")
		if reason != "" {
			if legacy := f.calls[choiceID+":legacy"]; legacy != nil && legacy.pending != nil {
				fn := jsonutil.MapAt(legacy.pending, "function")
				if f.renames.AllowsResponseTool(jsonutil.StringAt(fn, "name")) {
					name := jsonutil.StringAt(fn, "name")
					fn["name"], _ = f.renames.Resolve(name)
					if mapping, ok := f.renames.OfficialArgs(name); ok {
						fn["arguments"] = RemapArguments(jsonutil.StringAt(fn, "arguments"), mapping)
					}
					delta := jsonutil.MapAt(choice, "delta")
					if delta == nil {
						delta = map[string]any{}
						choice["delta"] = delta
					}
					delta["function_call"] = fn
					f.kept[choiceID] = true
				}
				legacy.pending = nil
			}
			if (reason == "tool_calls" || reason == "function_call") && !f.kept[choiceID] {
				choice["finish_reason"] = "stop"
			}
		}
	}
	return []map[string]any{payload}
}
func mergePendingChatCall(pending, call map[string]any) map[string]any {
	if pending == nil {
		pending = map[string]any{}
	}
	for k, v := range call {
		if k != "function" {
			pending[k] = v
		}
	}
	fn := jsonutil.MapAt(pending, "function")
	if fn == nil {
		fn = map[string]any{}
		pending["function"] = fn
	}
	for k, v := range jsonutil.MapAt(call, "function") {
		if k == "arguments" {
			fn[k] = jsonutil.StringAt(fn, k) + fmt.Sprint(v)
		} else if k == "name" {
			fn[k] = mergeToolName(jsonutil.StringAt(fn, k), fmt.Sprint(v))
		} else {
			fn[k] = v
		}
	}
	return pending
}
