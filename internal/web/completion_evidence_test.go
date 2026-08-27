package web

import "testing"

func TestClassifyToolOperation(t *testing.T) {
	cases := []struct {
		name string
		args string
		want []OperationalAction
	}{
		{"run", `{"cmd":"kubectl apply -f deploy.yaml"}`, []OperationalAction{ActionDeploy}},
		{"run", `{"cmd":"npm install express"}`, []OperationalAction{ActionInstall}},
		{"run", `{"cmd":"pytest tests/"}`, []OperationalAction{ActionVerify}},
		{"run", `{"cmd":"git push origin main"}`, []OperationalAction{ActionUpload}},
		{"run", `{"cmd":"rm -rf /tmp/build"}`, []OperationalAction{ActionDelete}},
		{"run", `{"cmd":"mkdir -p src/components"}`, []OperationalAction{ActionCreate}},
		{"write", `{"path":"config.json","content":"{\"port\":8080}"}`, []OperationalAction{ActionConfigure}},
		{"run", `{"cmd":"systemctl restart nginx"}`, []OperationalAction{ActionStart}},
		{"read", `{"path":"deploy.yaml"}`, []OperationalAction{ActionVerify}},
		{"run", `{"cmd":"go test ./..."}`, []OperationalAction{ActionVerify}},
		{"edit", `{"path":"main.go","content":"fix the bug"}`, []OperationalAction{ActionFix, ActionConfigure}},
	}
	for _, c := range cases {
		got := classifyToolOperation(c.name, c.args)
		if !actionSetEqual(got, c.want) {
			t.Errorf("classifyToolOperation(%q,%q) = %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

func actionSetEqual(got, want []OperationalAction) bool {
	if len(got) != len(want) {
		return false
	}
	gs := map[OperationalAction]bool{}
	for _, a := range got {
		gs[a] = true
	}
	for _, a := range want {
		if !gs[a] {
			return false
		}
	}
	return true
}

func TestCompletionClaims(t *testing.T) {
	cases := []struct {
		answer string
		want   []OperationalAction
	}{
		{"Deployment completed successfully", []OperationalAction{ActionDeploy}},
		{"I will deploy the service", nil}, // plan, not claim
		{"The service is now live", []OperationalAction{ActionDeploy}},
		{"All tests passed and deployment is live", []OperationalAction{ActionVerify, ActionDeploy}},
		{"I cannot confirm the deployment succeeded", nil}, // negation
		{"Installed, started, and verified successfully", []OperationalAction{ActionInstall, ActionStart, ActionVerify}},
		{"The package was installed", []OperationalAction{ActionInstall}},
	}
	for _, c := range cases {
		got := completionClaims(c.answer)
		if !actionSetEqual(got, c.want) {
			t.Errorf("completionClaims(%q) = %v, want %v", c.answer, got, c.want)
		}
	}
}

func TestCompletionEvidenceAllowsUpgraded(t *testing.T) {
	// Claim without any tool evidence: must be rejected.
	if completionEvidenceAllowsUpgraded("Deployment completed successfully", agentLedger{}) {
		t.Fatal("unsupported deployment claim allowed")
	}

	// Claim with matching tool evidence: must be allowed.
	ledger := agentLedger{Completed: []toolEvidence{{ID: "c1", Name: "run", Arguments: `{"cmd":"kubectl apply -f deploy.yaml"}`, Result: "deployment.apps/foo created"}}}
	if !completionEvidenceAllowsUpgraded("Deployment completed successfully", ledger) {
		t.Fatal("supported deployment claim rejected")
	}

	// Claim mismatching evidence: rejected.
	ledger2 := agentLedger{Completed: []toolEvidence{{ID: "c1", Name: "run", Arguments: `{"cmd":"go test ./..."}`, Result: "ok"}}}
	if completionEvidenceAllowsUpgraded("Deployment completed successfully", ledger2) {
		t.Fatal("deployment claim with only test evidence allowed")
	}

	// Pending calls always block.
	ledger3 := agentLedger{Pending: []toolEvidence{{ID: "p1", Name: "run", Arguments: `{"cmd":"kubectl apply"}`}}}
	if completionEvidenceAllowsUpgraded("Deployment completed successfully", ledger3) {
		t.Fatal("claim with pending evidence allowed")
	}

	// Generic completion claim with any successful operational evidence: allowed.
	ledger4 := agentLedger{Completed: []toolEvidence{{ID: "c1", Name: "write", Arguments: `{"path":"x","content":"y"}`, Result: "ok"}}}
	if !completionEvidenceAllowsUpgraded("All done", ledger4) {
		t.Fatal("generic completion with operational evidence rejected")
	}

	// Plain conversational answer with no claim: allowed (no tools needed).
	if !completionEvidenceAllowsUpgraded("Here is the code you asked for.", agentLedger{}) {
		t.Fatal("plain conversational answer rejected")
	}

	// Honest incomplete response: allowed.
	if !completionEvidenceAllowsUpgraded("I cannot confirm completion because no tool results were returned.", agentLedger{}) {
		t.Fatal("honest incomplete response rejected")
	}
}
