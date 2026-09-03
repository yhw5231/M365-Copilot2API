package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

// isBareNoToolMarker reports whether an assistant response consists of nothing
// but the internal tool-selection marker "NO_TOOL_NEEDED: ..." (see
// model_tool_router.go and unfulfilledClaimRepairText). Those lines are
// protocol tokens between the gateway and the upstream model, never a
// deliverable for the user: forwarding one as the final answer makes a client
// believe the round completed (with no work done), and the marker then leaks
// into the conversation history where the model keeps echoing it. A response
// that carries any other line is a real answer (possibly one that merely names
// the marker) and is not intercepted.
func isBareNoToolMarker(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	meaningful := 0
	for _, ln := range strings.Split(strings.TrimSpace(text), "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "no_tool_needed") {
			continue
		}
		meaningful++
	}
	return meaningful == 0
}

// noToolMarkerRecoveryText builds the one-shot recovery prompt for a round
// whose entire answer was the internal NO_TOOL_NEEDED marker. The marker means
// the model decided no tool call was needed; as a final answer it is a
// protocol artifact, not an answer. The recovery asks for a real natural
// language reply when no change is required, or an encoded tool call when one
// is, and tells the model to ignore the marker lines already present in the
// conversation history (which keeps the marker from self-reinforcing).
func noToolMarkerRecoveryText(toolMaps []map[string]any, prompt string) string {
	defs, _ := json.Marshal(toolMaps)
	return fmt.Sprintf(`Your previous reply was exactly the internal routing marker "NO_TOOL_NEEDED: no file or code change is being performed". That marker is a protocol token between the gateway and the model; it is never a valid answer for the user and will be rejected.

Note: any "NO_TOOL_NEEDED: ..." lines in the conversation history are the same protocol artifacts — ignore them.

Answer the user's request properly now:
1. If a file/code change is required and you can perform it, output ONLY a JSON tool call envelope that makes the change real:
   {"calls":[{"name":"function_name","arguments":{}}]}
   - read the target file first if you need its exact current content
   - for edit calls, copy old_string EXACTLY from the most recent read output; old_string and new_string MUST differ
   - validate every argument against FUNCTION_DEFINITIONS
2. If genuinely no change is required, reply in natural language explaining why — do NOT repeat "NO_TOOL_NEEDED".

User request and evidence:
%s

FUNCTION_DEFINITIONS:
%s`, prompt, string(defs))
}
