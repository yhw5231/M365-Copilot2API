package chathub

import "encoding/json"

// Tool represents a tool declaration from the client.
type Tool struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function,omitempty"`
}

// compatibilityOnlyTools are tools that must execute on the caller's local
// machine. Registering them in M365's native plugin list can make ChatHub
// bind them to a hosted Linux shell or filesystem. They are kept in the
// fenced-call compatibility prompt, which the gateway converts back into a
// Responses function_call for Codex (or other clients) to execute locally.
var compatibilityOnlyTools = map[string]bool{
	"exec_command": true,
	"write_stdin":  true,
	"view_image":   true,
}

// clientPlugins builds the M365 native plugin list from the client's declared
// tools. Tools in the compatibilityOnly set are excluded to prevent ChatHub
// from binding them to a hosted execution environment; they remain available
// through the fenced-call compatibility path in toolProtocolPrompt.
func clientPlugins(tools []Tool, mcpServerURL string) []any {
	plugins := make([]any, 0, len(tools)+1)
	if mcpServerURL != "" {
		plugins = append(plugins, map[string]any{
			"Id":                "mcp-gateway",
			"Source":            "MCPServer",
			"Description":       "MCP Gateway tools",
			"Transport":         "mcp",
			"TransportUrl":      mcpServerURL,
			"TransportProtocol": "https://copilot.microsoft.com/schemas/plugins/local/transport/1.0",
		})
	}
	for _, t := range tools {
		var f struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		// Skip tools that must execute on the caller's local machine
		// to prevent ChatHub from binding them to hosted execution.
		if compatibilityOnlyTools[f.Name] {
			continue
		}
		plugins = append(plugins, map[string]any{"Id": f.Name, "Source": "API", "Description": f.Description, "Parameters": f.Parameters})
	}
	return plugins
}

// IsCompatibilityOnlyTool reports whether the named tool belongs to the
// compatibility-only set (must execute on the caller's local machine).
func IsCompatibilityOnlyTool(name string) bool {
	return compatibilityOnlyTools[name]
}
