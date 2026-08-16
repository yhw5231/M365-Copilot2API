package outbound

import (
	"errors"
	"testing"
)

var errConnBroken = errors.New("connection broken")

func mustPool(t *testing.T, raw []string) *Pool {
	t.Helper()
	p, err := NewPool(raw)
	if err != nil {
		t.Fatalf("NewPool(%v): %v", raw, err)
	}
	return p
}

func rawOf(e *poolEntry) string {
	if e == nil {
		return ""
	}
	return e.raw
}

// 账号粘性 + 独占：账号1 一直使用 A 节点；账号2 不得使用 A 节点。
func TestPoolPickForPinsAccountToNode(t *testing.T) {
	p := mustPool(t, []string{"http://a.example", "http://b.example"})
	a1 := rawOf(p.pickFor("account-1"))
	a2 := rawOf(p.pickFor("account-1"))
	if a1 == "" || a1 != a2 {
		t.Fatalf("account-1 must keep its bound node, got %q then %q", a1, a2)
	}
	b1 := rawOf(p.pickFor("account-2"))
	if b1 == "" {
		t.Fatal("account-2 got no node")
	}
	if b1 == a1 {
		t.Fatalf("account-2 must not use account-1's bound node %q", a1)
	}
	// 再验证一次：两个账号各自稳定。
	if rawOf(p.pickFor("account-2")) != b1 {
		t.Fatalf("account-2 lost its binding")
	}
	if rawOf(p.pickFor("account-1")) != a1 {
		t.Fatalf("account-1 lost its binding")
	}
}

// 故障转移：绑定节点故障时优先使用没有绑定账号的节点。
func TestPoolPickForPrefersUnboundNodeOnFailure(t *testing.T) {
	p := mustPool(t, []string{"http://a.example", "http://b.example", "http://c.example"})
	a1 := rawOf(p.pickFor("account-1"))
	b1 := rawOf(p.pickFor("account-2"))
	if a1 == b1 {
		t.Fatalf("accounts collided on %q", a1)
	}
	// a1 故障 → account-1 应迁移到未绑定的 c 节点，而不是占用 account-2 的 b1。
	p.mark(a1, errConnBroken)
	got := rawOf(p.pickFor("account-1"))
	if got == "" {
		t.Fatal("no node after failure")
	}
	if got != "http://c.example" {
		t.Fatalf("expected unbound node c, got %q", got)
	}
	// account-2 的绑定不受影响。
	if rawOf(p.pickFor("account-2")) != b1 {
		t.Fatalf("account-2 binding changed")
	}
}

// 兜底语义：没有空闲健康节点时不允许借用其他账号绑定的节点；
// pickFor 返回 nil，由上层把请求切换到绑定健康节点的账号（FailoverTarget）。
func TestPoolPickForDoesNotBorrowBoundNode(t *testing.T) {
	p := mustPool(t, []string{"http://a.example", "http://b.example"})
	a1 := rawOf(p.pickFor("account-1"))
	b1 := rawOf(p.pickFor("account-2"))
	if a1 == "" || b1 == "" || a1 == b1 {
		t.Fatalf("accounts collided: a=%q b=%q", a1, b1)
	}
	// 两个节点都被绑定且健康 → account-3 得不到节点（不能借用）。
	if e := p.pickFor("account-3"); e != nil {
		t.Fatalf("pickFor must not borrow a bound node, got %q", e.raw)
	}
	// 正确动作：本请求改用"绑定健康节点的账号"执行。
	target, ok := p.FailoverTarget("account-3")
	if !ok || (target != "account-1" && target != "account-2") {
		t.Fatalf("FailoverTarget=%q,%t want account-1 or account-2", target, ok)
	}
}

// FailoverTarget 排除自身；只返回绑定健康节点的账号。
func TestPoolFailoverTargetExcludesSelfAndUnhealthy(t *testing.T) {
	p := mustPool(t, []string{"http://a.example", "http://b.example"})
	a1 := rawOf(p.pickFor("account-1"))
	rawOf(p.pickFor("account-2"))
	// account-1 的绑定节点故障 → 自身排除，返回 account-2（其节点健康）。
	p.mark(a1, errConnBroken)
	target, ok := p.FailoverTarget("account-1")
	if !ok || target != "account-2" {
		t.Fatalf("FailoverTarget(account-1)=%q,%t want account-2", target, ok)
	}
	// 全部节点故障 → 没有可切换的账号。
	p.mark("http://b.example", errConnBroken)
	if _, ok := p.FailoverTarget("account-1"); ok {
		t.Fatal("expected no failover target when all nodes are down")
	}
}

// 全故障：所有节点都在冷却期时返回 nil。
func TestPoolPickForAllFailed(t *testing.T) {
	p := mustPool(t, []string{"http://a.example", "http://b.example"})
	pickAll := []string{"http://a.example", "http://b.example"}
	for _, raw := range pickAll {
		p.mark(raw, errConnBroken)
	}
	if e := p.pickFor("account-1"); e != nil {
		t.Fatalf("expected nil with all nodes in cooldown, got %q", e.raw)
	}
}

// 删除节点会释放该节点的账号绑定。
func TestPoolRemoveReleasesBindings(t *testing.T) {
	p := mustPool(t, []string{"http://a.example", "http://b.example"})
	a1 := rawOf(p.pickFor("account-1"))
	p.Remove(a1)
	got := rawOf(p.pickFor("account-1"))
	if got == a1 {
		t.Fatalf("account-1 still bound to removed node %q", a1)
	}
	if got == "" {
		t.Fatal("no replacement node after remove")
	}
}

// List 暴露绑定关系，便于控制台诊断。
func TestPoolListReportsBoundAccounts(t *testing.T) {
	p := mustPool(t, []string{"http://a.example", "http://b.example"})
	p.pickFor("account-1")
	sawBucket := false
	for _, item := range p.List() {
		if bounds, ok := item["boundAccounts"].([]string); ok && len(bounds) > 0 {
			if bounds[0] != "account-1" {
				t.Fatalf("unexpected boundAccounts %v", bounds)
			}
			sawBucket = true
		}
	}
	if !sawBucket {
		t.Fatal("no boundAccounts reported")
	}
}