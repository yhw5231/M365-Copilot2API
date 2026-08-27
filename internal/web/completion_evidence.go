package web

import (
	"regexp"
)

// OperationalAction represents a concrete operation a tool call can perform.
// These are inferred from the tool's NAME and ARGUMENTS (never from output),
// preventing a read-only tool result containing "deployed successfully" from
// manufacturing deployment evidence.
type OperationalAction string

const (
	ActionDeploy    OperationalAction = "deploy"
	ActionFix       OperationalAction = "fix"
	ActionInstall   OperationalAction = "install"
	ActionVerify    OperationalAction = "verify"
	ActionUpload    OperationalAction = "upload"
	ActionDelete    OperationalAction = "delete"
	ActionCreate    OperationalAction = "create"
	ActionConfigure OperationalAction = "configure"
	ActionStart     OperationalAction = "start"
	ActionComplete  OperationalAction = "complete"
)

var allOperationalActions = []OperationalAction{
	ActionDeploy, ActionFix, ActionInstall, ActionVerify,
	ActionUpload, ActionDelete, ActionCreate, ActionConfigure, ActionStart,
}

// operationPatterns classify a tool call by its name and argument text into
// operational actions. The classification is based on the tool's intended
// operation (from its name + arguments), not on the tool output, so a read
// tool whose result text says "deployed successfully" does not create
// deployment evidence.
var operationPatterns = map[OperationalAction][]*regexp.Regexp{
	ActionDeploy: {
		regexp.MustCompile(`(?i)\b(deploy|deployment|release)\b`),
		regexp.MustCompile(`(?i)\bkubectl\s+(apply|rollout)\b`),
		regexp.MustCompile(`(?i)\bdocker(?:\s+compose|\s+stack)?\s+(up|deploy)\b`),
	},
	ActionFix: {
		regexp.MustCompile(`(?i)\b(fix|repair|patch)\b`),
		regexp.MustCompile(`(?i)apply[_-]?patch`),
	},
	ActionInstall: {
		regexp.MustCompile(`(?i)\b(install|installer|setup)\b`),
		regexp.MustCompile(`(?i)\b(apt(?:-get)?|dnf|yum|pip\d*|npm|pnpm|yarn|winget|choco|brew)\s+install\b`),
	},
	ActionVerify: {
		regexp.MustCompile(`(?i)\b(verify|validate|tests?|checks?|health|doctor|audit|inspect|read|view|stat)\b`),
		regexp.MustCompile(`(?i)\b(go|npm|pnpm|yarn|cargo|pytest|vitest)\s+(run\s+)?test\b`),
	},
	ActionUpload: {
		regexp.MustCompile(`(?i)\b(upload|publish|push|sync)\b`),
		regexp.MustCompile(`(?i)git\s+push`),
		regexp.MustCompile(`(?i)\b(scp|rsync|rclone)\b`),
	},
	ActionDelete: {
		regexp.MustCompile(`(?i)\b(delete|remove|cleanup|clean|purge)\b`),
		regexp.MustCompile(`(?i)\brm\s+(?:-[^\s]+\s+)*(?:--\s+)?[^\s]`),
		regexp.MustCompile(`(?i)\bRemove-Item\b`),
	},
	ActionCreate: {
		regexp.MustCompile(`(?i)\b(create|provision|scaffold|mkdir)\b`),
		regexp.MustCompile(`(?i)\b(mkdir|New-Item)\b`),
	},
	ActionConfigure: {
		regexp.MustCompile(`(?i)\b(configure|config|edit|write|update|modify|save)\b`),
		regexp.MustCompile(`(?i)\b(Set-Content|Add-Content)\b`),
	},
	ActionStart: {
		regexp.MustCompile(`(?i)\b(start|restart|launch|run_service)\b`),
		regexp.MustCompile(`(?i)\bsystemctl\s+(start|restart|reload)\b`),
		regexp.MustCompile(`(?i)\bStart-Service\b`),
	},
}

// claimPatterns detect strong operational completion claims in the assistant's
// final answer. A claim is a statement that an operation has been completed,
// not a plan or hypothetical.
var claimPatterns = map[OperationalAction][]*regexp.Regexp{
	ActionDeploy: {
		regexp.MustCompile(`(?i)\b(successfully\s+)?deployed\b`),
		regexp.MustCompile(`(?i)\bdeployment\b[^.!?;]{0,40}\b(complete|completed|successful|live|is now live)\b`),
		regexp.MustCompile(`(?i)\b(is|went|is now)\s+(now\s+)?live\b`),
		regexp.MustCompile(`(?i)\b(is|was)\s+(rolled\s+out|shipped|launched|published)\b`),
	},
	ActionFix: {
		regexp.MustCompile(`(?i)\b(successfully\s+)?(fixed|repaired|resolved)\b`),
		regexp.MustCompile(`(?i)\b(fix|repair|remediation)\s+(is|was|has been)\s+(complete|completed|successful)`),
	},
	ActionInstall: {
		regexp.MustCompile(`(?i)\b(successfully\s+)?installed\b`),
		regexp.MustCompile(`(?i)installation\s+(is|was|has been)\s+(complete|completed|successful)`),
	},
	ActionVerify: {
		regexp.MustCompile(`(?i)\b(successfully\s+)?verified\b`),
		regexp.MustCompile(`(?i)\b(tests?|checks?|validation|verification)\s+(passed|succeeded|green|are green|is green|all passed)\b`),
		regexp.MustCompile(`(?i)\ball\s+tests?\s+(pass|passed|succeeded)\b`),
	},
	ActionUpload: {
		regexp.MustCompile(`(?i)\b(successfully\s+)?(uploaded|published|pushed|synced)\b`),
		regexp.MustCompile(`(?i)(upload|publication|push|sync)\s+(is|was|has been)\s+(complete|completed|successful)`),
	},
	ActionDelete: {
		regexp.MustCompile(`(?i)\b(successfully\s+)?(deleted|removed|cleaned|purged)\b`),
		regexp.MustCompile(`(?i)(deletion|removal|cleanup)\s+(is|was|has been)\s+(complete|completed|successful)`),
	},
	ActionCreate: {
		regexp.MustCompile(`(?i)\b(successfully\s+)?created\b`),
		regexp.MustCompile(`(?i)creation\s+(is|was|has been)\s+(complete|completed|successful)`),
	},
	ActionConfigure: {
		regexp.MustCompile(`(?i)\b(successfully\s+)?(configured|updated|modified|written|saved)\b`),
		regexp.MustCompile(`(?i)(configuration|update|modification)\s+(is|was|has been)\s+(complete|completed|successful)`),
	},
	ActionStart: {
		regexp.MustCompile(`(?i)\b(successfully\s+)?(started|restarted|launched|reloaded)\b`),
		regexp.MustCompile(`(?i)(startup|restart|launch|reload)\s+(is|was|has been)\s+(complete|completed|successful)`),
	},
	ActionComplete: {
		regexp.MustCompile(`(?i)\b(all done|everything\s+(is\s+)?(done|complete)|task\s+(is|was|has been)\s+(done|complete|completed)|completed\s+successfully)\b`),
	},
}

// nonAssertiveContext detects patterns that make a claim non-assertive
// (negation, hypotheticals, plans, quotes).
func nonAssertiveContext(text string, start, end int) bool {
	before := text[beforePos(0, start-80):start]
	after := text[end:afterPos(len(text), end+18)]

	// Negation: not, never, cannot, failed to, etc.
	negationRe := regexp.MustCompile(`(?i)\b(not|never|cannot|can't|unable to|failed to|didn't|hasn't|haven't|wasn't|isn't|cannot confirm|can't confirm)\b[^,.!?;]{0,20}$`)
	if negationRe.MatchString(before) {
		return true
	}

	// Plans, requirements, hypotheticals
	planRe := regexp.MustCompile(`(?i)\b(if|when|once|unless|provided|assuming|will|would|could|should|can|may|might|plan(s|ned)? to|intend(s|ed)? to|need(s|ed)? to|going to|must)\b[^,.!?;]{0,35}$`)
	if planRe.MatchString(before) {
		return true
	}

	_ = after // future use for post-claim context
	return false
}

// readOnlyToolNames are tool names that can only inspect, never mutate or
// perform operations. When a tool call uses one of these names, it is always
// classified as verify and never as any other operational action.
var readOnlyToolNames = map[string]bool{
	"read": true, "view": true, "cat": true, "ls": true, "dir": true,
	"glob": true, "grep": true, "stat": true, "head": true, "tail": true,
	"find": true, "list": true, "get": true, "workspace_read": true,
}

// classifyToolOperation infers the operational actions for a completed tool
// call based on its NAME and ARGUMENTS. The classification never reads the
// tool output, so it cannot be contaminated by a read-only result that
// happens to contain "deployed successfully".
//
// Read-only tools (read, view, ls, stat, glob, grep, etc.) always classify
// as verify only — they can perform inspection but never a mutation or
// deployment.
func classifyToolOperation(name, arguments string) []OperationalAction {
	// Read-only tools can only be verify.
	if readOnlyToolNames[name] {
		return []OperationalAction{ActionVerify}
	}
	var actions []OperationalAction
	text := name + "\n" + arguments
	for _, action := range allOperationalActions {
		for _, re := range operationPatterns[action] {
			if re.MatchString(text) {
				actions = append(actions, action)
				break
			}
		}
	}
	return actions
}

// completionClaims extracts strong operational completion claims from the
// assistant's final answer. Claims that are negated, hypothetical, or planned
// are excluded.
func completionClaims(answer string) []OperationalAction {
	seen := map[OperationalAction]bool{}
	var claims []OperationalAction
	// Check specific actions first
	for _, action := range allOperationalActions {
		for _, re := range claimPatterns[action] {
			for _, match := range re.FindAllStringIndex(answer, -1) {
				if !nonAssertiveContext(answer, match[0], match[1]) {
					if !seen[action] {
						seen[action] = true
						claims = append(claims, action)
					}
					break
				}
			}
			if seen[action] {
				break
			}
		}
	}
	// Check the generic "complete" claim
	for _, re := range claimPatterns[ActionComplete] {
		for _, match := range re.FindAllStringIndex(answer, -1) {
			if !nonAssertiveContext(answer, match[0], match[1]) {
				if !seen[ActionComplete] {
					seen[ActionComplete] = true
					claims = append(claims, ActionComplete)
				}
				break
			}
		}
		if seen[ActionComplete] {
			break
		}
	}
	// If there are specific claims plus "complete", remove "complete"
	// (the specific claim is more precise).
	if seen[ActionComplete] {
		hasSpecific := false
		for _, action := range allOperationalActions {
			if seen[action] {
				hasSpecific = true
				break
			}
		}
		if hasSpecific {
			claims = removeAction(claims, ActionComplete)
		}
	}
	return claims
}

func removeAction(actions []OperationalAction, action OperationalAction) []OperationalAction {
	var out []OperationalAction
	for _, a := range actions {
		if a != action {
			out = append(out, a)
		}
	}
	return out
}

// completionEvidenceAllowsUpgraded is the action-classification-aware version
// of completionEvidenceAllows. It classifies each completed tool call into
// operational actions, detects claims in the answer, and validates each claim
// against the classified evidence. It falls back to the original behavior for
// generic claims or no-claim answers.
func completionEvidenceAllowsUpgraded(answer string, l agentLedger) bool {
	// Pending calls always block.
	if len(l.Pending) > 0 {
		return false
	}

	// Detect operational claims in the answer.
	claims := completionClaims(answer)
	if len(claims) == 0 {
		// No claims: fall back to the original behavior.
		return completionEvidenceAllows(answer, l)
	}

	// If there are no completed tools, any claim is unsupported.
	if len(l.Completed) == 0 {
		return false
	}

	// Classify each completed tool call into actions.
	// A tool call supports a claim if it is classified as that action
	// and did not fail.
	classifiedActions := map[OperationalAction]bool{}
	for _, ev := range l.Completed {
		for _, action := range classifyToolOperation(ev.Name, ev.Arguments) {
			if !ev.Failed {
				classifiedActions[action] = true
			}
		}
	}

	// "complete" is special: it requires at least one successful operational
	// action to be classified.
	for _, claim := range claims {
		if claim == ActionComplete {
			if len(classifiedActions) == 0 {
				return false
			}
			continue
		}
		if !classifiedActions[claim] {
			return false
		}
	}
	return true
}

func beforePos(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func afterPos(a, b int) int {
	if a < b {
		return a
	}
	return b
}