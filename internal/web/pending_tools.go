package web

import (
	"encoding/json"
	"strings"
	"time"
)

// pendingToolCall records a function_call the gateway emitted in a previous
// Responses turn. Codex (and other stateless Responses clients) sometimes send
// function_call_output without replaying the matching function_call and without
// previous_response_id. The registry lets the gateway validate that output
// against the actual call instead of failing with "unexpected tool result".
type pendingToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	CreatedAt time.Time       `json:"created_at"`
}

// pendingToolTTL bounds how long a pending call remains resolvable after the
// turn that produced it. Codex's default agent step timeout is well under this.
const pendingToolTTL = 10 * time.Minute

// recordPendingToolCalls stores the given tool calls as pending under the
// tenant's registry, replacing any prior entries with the same call id.
func (s *Server) recordPendingToolCalls(tenant string, calls []detectedToolCall) {
	if len(calls) == 0 {
		return
	}
	s.pendingToolsMu.Lock()
	defer s.pendingToolsMu.Unlock()
	bucket := s.pendingTools[tenant]
	if bucket == nil {
		bucket = map[string]pendingToolCall{}
	}
	for _, c := range calls {
		if c.ID == "" || c.Name == "" {
			continue
		}
		bucket[c.ID] = pendingToolCall{Name: c.Name, Arguments: c.Arguments, CreatedAt: time.Now()}
	}
	s.pendingTools[tenant] = bucket
	s.prunePendingToolsLocked(tenant)
}

// pendingToolCallsFor returns a copy of the tenant's pending calls.
func (s *Server) pendingToolCallsFor(tenant string) map[string]pendingToolCall {
	s.pendingToolsMu.Lock()
	defer s.pendingToolsMu.Unlock()
	s.prunePendingToolsLocked(tenant)
	out := map[string]pendingToolCall{}
	for k, v := range s.pendingTools[tenant] {
		out[k] = v
	}
	return out
}

// prunePendingToolsLocked drops expired entries for the tenant. Caller holds
// pendingToolsMu.
func (s *Server) prunePendingToolsLocked(tenant string) {
	bucket := s.pendingTools[tenant]
	if len(bucket) == 0 {
		delete(s.pendingTools, tenant)
		return
	}
	cutoff := time.Now().Add(-pendingToolTTL)
	for id, call := range bucket {
		if call.CreatedAt.Before(cutoff) {
			delete(bucket, id)
		}
	}
	if len(bucket) == 0 {
		delete(s.pendingTools, tenant)
	}
}

// consumePendingToolCall looks up and deletes a pending call by id. The bool
// reports whether the call was found and still within TTL.
func (s *Server) consumePendingToolCall(tenant, callID string) (pendingToolCall, bool) {
	s.pendingToolsMu.Lock()
	defer s.pendingToolsMu.Unlock()
	bucket := s.pendingTools[tenant]
	if bucket == nil {
		return pendingToolCall{}, false
	}
	call, ok := bucket[callID]
	if !ok {
		return pendingToolCall{}, false
	}
	if time.Since(call.CreatedAt) > pendingToolTTL {
		delete(bucket, callID)
		return pendingToolCall{}, false
	}
	delete(bucket, callID)
	if len(bucket) == 0 {
		delete(s.pendingTools, tenant)
	}
	return call, true
}

// assistantCallIDs extracts call ids declared by assistant tool_calls in the
// given message list.
func assistantCallIDs(messages []oaiMsg) map[string]bool {
	out := map[string]bool{}
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.ToolCalls {
			id, _ := call["id"].(string)
			if id != "" {
				out[id] = true
			}
		}
	}
	return out
}

// restoreStatelessToolCalls back-fills assistant function_call messages for any
// tool results whose call id is not declared in the request itself but is
// resolvable from the tenant's pending registry. This is the stateless
// Responses continuation path: the client omits the replayed function_call and
// previous_response_id, relying on the gateway to remember the pending call.
// The injected call is placed immediately before its tool result so the
// conversation stays causally ordered and validateToolConversation accepts it.
// The call is consumed from the registry once.
func (s *Server) restoreStatelessToolCalls(tenant string, messages []oaiMsg) []oaiMsg {
	declared := assistantCallIDs(messages)
	var out []oaiMsg
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" && !declared[m.ToolCallID] {
			if call, ok := s.consumePendingToolCall(tenant, m.ToolCallID); ok {
				callID := m.ToolCallID
				args := call.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				name := strings.TrimSpace(call.Name)
				if name != "" {
					out = append(out, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": string(args)}}}})
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// collectDetectedToolCalls extracts detectedToolCall entries from an assistant
// message's tool_calls, for recording into the pending registry.
func collectDetectedToolCalls(messages []oaiMsg) []detectedToolCall {
	var calls []detectedToolCall
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.ToolCalls {
			id, _ := c["id"].(string)
			if id == "" {
				continue
			}
			var name string
			var args json.RawMessage
			if f, ok := c["function"].(map[string]any); ok {
				name, _ = f["name"].(string)
				if a, ok := f["arguments"].(string); ok {
					args = json.RawMessage(a)
				} else if a, ok := f["arguments"].(json.RawMessage); ok {
					args = a
				}
			}
			if name == "" {
				continue
			}
			calls = append(calls, detectedToolCall{ID: id, Type: "function", Name: name, Arguments: args})
		}
	}
	return calls
}
