package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"m365-copilot2api/internal/mcp"
)

// registerGoalMCPHandler wires the goal-state tools to the web layer's task
// ledger through the global MCP call handler, so session-less MCP clients can
// read/update the goal of the downstream session bound to their API key.
func (s *Server) registerGoalMCPHandler() {
	mcp.GlobalToolCallHandler = func(ctx context.Context, apiKey, name string, args map[string]any) (mcp.CallResult, error) {
		goalTools := map[string]bool{"create_goal": true, "get_goal": true, "update_goal": true}
		if !goalTools[name] {
			return mcp.CallResult{}, fmt.Errorf("tool %s not available on this session", name)
		}
		sessionID := s.findSessionByAPIKey(apiKey)
		if sessionID == "" {
			return mcp.CallResult{}, fmt.Errorf("no active session for this API key; call a chat endpoint first to bind a session")
		}
		binding, ok := s.sessionResolver.GetSession(sessionID)
		if !ok {
			return mcp.CallResult{}, fmt.Errorf("session not found")
		}
		task := binding.Task
		created := false
		if task == nil {
			task = &taskLedger{UpdatedAt: time.Now().UTC()}
			created = true
		}
		// Run the goal action against the ledger, mirroring applyGoalToolEvidence.
		var resultText string
		switch name {
		case "create_goal":
			obj, _ := args["objective"].(string)
			if obj == "" {
				return mcp.CallResult{}, fmt.Errorf("create_goal requires objective")
			}
			task.OriginalGoal = compactTaskText(obj, taskGoalMax)
			if task.GoalID == "" {
				task.GoalID = "goal-" + shortID()
			}
			resultText = fmt.Sprintf("goal created: id=%s status=active", task.GoalID)
		case "get_goal":
			return mcp.CallResult{
				Content: []map[string]any{{"type": "text", "text": goalStateText(task)}},
			}, nil
		case "update_goal":
			action, _ := args["action"].(string)
			switch strings.ToLower(action) {
			case "complete":
				task.markComplete(fmt.Sprint(args["blocked_reason"]))
			case "blocked":
				if task.Status == "" || task.Status == "active" {
					task.Status = taskStatusBlocked
					task.CompletedReason = fmt.Sprint(args["blocked_reason"])
					now := time.Now().UTC()
					task.CompletedAt = &now
					task.UpdatedAt = now
				}
			case "paused", "pause":
				if task.Status == "" || task.Status == "active" {
					task.Status = taskStatusPaused
					now := time.Now().UTC()
					task.CompletedAt = &now
					task.UpdatedAt = now
				}
			case "resume":
				if task.Status == taskStatusPaused || task.Status == taskStatusBlocked {
					task.Status = ""
					task.CompletedAt = nil
					task.CompletedReason = ""
					task.UpdatedAt = time.Now().UTC()
				}
			case "edit":
				if v, ok := args["objective"].(string); ok && strings.TrimSpace(v) != "" {
					task.OriginalGoal = compactTaskText(v, taskGoalMax)
				}
			default:
				return mcp.CallResult{}, fmt.Errorf("update_goal action must be edit, pause, resume, complete, or blocked")
			}
			resultText = goalStateText(task)
		}
		if created || binding.Task == nil {
			s.sessionResolver.SetTask(sessionID, task)
		} else if name != "get_goal" {
			s.sessionResolver.SetTask(sessionID, task)
		}
		return mcp.CallResult{
			Content: []map[string]any{{"type": "text", "text": resultText}},
		}, nil
	}
}

func goalStateText(task *taskLedger) string {
	status := "active"
	if task != nil && task.Status != "" {
		status = task.Status
	}
	if task == nil {
		return `{"status":"none"}`
	}
	return fmt.Sprintf(`{"goal_id":%q,"status":%q,"objective":%q}`, task.GoalID, status, task.OriginalGoal)
}

func shortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// registerGoalMCPTools exposes the goal-state tools through the MCP tool
// registry so MCP-aware clients (Claude Code, custom tools) can discover a
// goal-state read/update entry on this gateway.
func registerGoalMCPTools() {
	mcp.GlobalToolRegistry.MergeTools([]mcp.Tool{
		{
			Name:        "create_goal",
			Description: "Create a persistent server-side goal (long-running objective) bound to the current downstream session. The gateway records the goal id and objective so later goal rounds and the /v1/goal entry can read and update its state. Do not create a goal for routine single-turn work.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"objective"},
				"properties": map[string]any{
					"objective":      map[string]any{"type": "string", "description": "The long-running completion objective."},
					"max_goal_rounds": map[string]any{"type": "integer"},
				},
			},
		},
		{
			Name:        "get_goal",
			Description: "Read the current server-side goal state for the downstream session: goal_id, status (active/complete/blocked/paused), objective, executed steps, remaining steps and completion reason. Returns status none when no goal ledger exists.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "update_goal",
			Description: "Update the server-side goal state. action=complete closes the goal and persists completion; blocked/paused set a waiting state; resume re-opens a paused/blocked goal. The gateway mirrors this into the task ledger so the model stops treating the goal as active.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"action"},
				"properties": map[string]any{
					"action":         map[string]any{"type": "string", "enum": []string{"edit", "pause", "resume", "complete", "blocked"}},
					"goal_id":        map[string]any{"type": "string"},
					"revision":       map[string]any{"type": "integer"},
					"objective":      map[string]any{"type": "string"},
					"max_goal_rounds": map[string]any{"type": "integer"},
					"blocked_reason": map[string]any{"type": "string"},
				},
			},
		},
	})
}

// goalStateHandler provides a read/update entry for the server-side goal state
// that the session's task ledger tracks.  The agent complained "当前会话没有可用的
// 目标状态读取或更新入口" — this endpoint is that entry.
//
//	GET  /v1/goal          — returns the current goal ledger as JSON
//	POST /v1/goal          — updates the goal (action=complete|blocked|paused|resume)
func (s *Server) handleGoalState(w http.ResponseWriter, r *http.Request) {
	apiKey := extractAPIKey(r)
	if apiKey == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "valid API key required")
		return
	}
	if !s.validAPIKey(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "invalid API key")
		return
	}

	// Resolve the downstream session that owns the goal.
	// Prefer the explicit session ID header (session_id / x-session-id);
	// fallback to the API key's most recent session.
	sessionID := sessionIDFromRequest(r)
	if sessionID == "" {
		sessionID = r.URL.Query().Get("session_id")
	}
	if sessionID == "" {
		sessionID = s.findSessionByAPIKey(apiKey)
	}
	if sessionID == "" {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "no active session for this API key")
		return
	}

	binding, ok := s.sessionResolver.GetSession(sessionID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		goalJSON(w, binding.Task)
	case http.MethodPost:
		var req struct {
			Action   string `json:"action"`
			GoalID   string `json:"goal_id"`
			Reason   string `json:"reason"`
			Revision int    `json:"revision,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json: "+err.Error())
			return
		}
		task := binding.Task
		if task == nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "no goal ledger for this session")
			return
		}
		if req.GoalID != "" && task.GoalID != "" && req.GoalID != task.GoalID {
			writeOpenAIError(w, http.StatusConflict, "goal_id_mismatch", fmt.Sprintf("goal_id mismatch: got %q want %q", req.GoalID, task.GoalID))
			return
		}
		switch strings.ToLower(req.Action) {
		case "complete":
			task.markComplete(req.Reason)
		case "blocked":
			if task.Status == "" || task.Status == "active" {
				task.Status = taskStatusBlocked
				task.CompletedReason = req.Reason
				now := time.Now().UTC()
				task.CompletedAt = &now
				task.UpdatedAt = now
			}
		case "paused", "pause":
			if task.Status == "" || task.Status == "active" {
				task.Status = taskStatusPaused
				now := time.Now().UTC()
				task.CompletedAt = &now
				task.UpdatedAt = now
			}
		case "resume":
			if task.Status == taskStatusPaused || task.Status == taskStatusBlocked {
				task.Status = ""
				task.CompletedAt = nil
				task.CompletedReason = ""
				task.UpdatedAt = time.Now().UTC()
			}
		default:
			writeOpenAIError(w, http.StatusBadRequest, "invalid_action", "action must be complete, blocked, paused, or resume")
			return
		}
		s.sessionResolver.SetTask(sessionID, task)
		goalJSON(w, task)
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET or POST")
	}
}

// findSessionByAPIKey scans the session resolver for a session bound to the
// given API key and returns the most recently used session id.
func (s *Server) findSessionByAPIKey(apiKey string) string {
	if s == nil || s.sessionResolver == nil || apiKey == "" {
		return ""
	}
	// The sessionResolver does not index by API key, so we scan.
	// This is a bounded list (default max 1000, TTL-culled).
	for _, b := range s.sessionResolver.ListSessions() {
		if b.APIKey != apiKey {
			continue
		}
		return b.SessionID
	}
	return ""
}

func goalJSON(w http.ResponseWriter, task *taskLedger) {
	w.Header().Set("Content-Type", "application/json")
	if task == nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "none"})
		return
	}
	status := task.Status
	if status == "" {
		status = "active"
	}
	out := map[string]any{
		"goal_id":    task.GoalID,
		"status":     status,
		"objective":  task.OriginalGoal,
		"executed":   task.Executed,
		"remaining":  task.Remaining,
		"failures":   task.Failures,
		"updated_at": task.UpdatedAt,
	}
	if task.CompletedAt != nil {
		out["completed_at"] = task.CompletedAt
	}
	if task.CompletedReason != "" {
		out["completed_reason"] = task.CompletedReason
	}
	json.NewEncoder(w).Encode(out)
}