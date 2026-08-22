package chathub

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestConnPoolKeysConnectionsBySession(t *testing.T) {
	pool := NewConnPool(nil, nil)
	connA := &websocket.Conn{}
	connB := &websocket.Conn{}

	pool.conns[pool.key("oid", "tid", "session-a")] = &pooledConn{conn: connA, created: time.Now()}
	pool.conns[pool.key("oid", "tid", "session-b")] = &pooledConn{conn: connB, created: time.Now()}

	if got := pool.conns[pool.key("oid", "tid", "session-a")]; got == nil || got.conn != connA {
		t.Fatal("session-a did not resolve to its own pooled connection")
	}
	if got := pool.conns[pool.key("oid", "tid", "session-b")]; got == nil || got.conn != connB {
		t.Fatal("session-b did not resolve to its own pooled connection")
	}
	if pool.key("oid", "tid", "session-a") == pool.key("oid", "tid", "session-b") {
		t.Fatal("different sessions produced the same connection-pool key")
	}
}

func TestConnPoolKeyReusesSameSession(t *testing.T) {
	pool := NewConnPool(nil, nil)
	first := pool.key("oid", "tid", "session-a")
	second := pool.key("oid", "tid", "session-a")
	if first != second {
		t.Fatalf("same session produced different pool keys: %q and %q", first, second)
	}
}
