package chathub

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type pooledConn struct {
	conn    *websocket.Conn
	created time.Time
}

type ConnPool struct {
	mu      sync.Mutex
	conns   map[string]*pooledConn // key = oid|tid
	dialer  *websocket.Dialer
	header  http.Header
	baseURL string // pre-built URL without session/conversation IDs
	stop    chan struct{}
}

func NewConnPool(dialer *websocket.Dialer, header http.Header) *ConnPool {
	return &ConnPool{
		conns:  make(map[string]*pooledConn),
		dialer: dialer,
		header: header,
		stop:   make(chan struct{}),
	}
}

func (p *ConnPool) key(oid, tid, sessionID string) string {
	return oid + "|" + tid + "|" + sessionID
}

func (p *ConnPool) Take(ctx context.Context, oid, tid, sessionID, wsURL string) (*websocket.Conn, bool, error) {
	key := p.key(oid, tid, sessionID)
	p.mu.Lock()
	pc := p.conns[key]
	if pc != nil {
		delete(p.conns, key)
	}
	p.mu.Unlock()
	if pc != nil {
		_ = pc.conn.SetReadDeadline(time.Time{})
		_ = pc.conn.SetWriteDeadline(time.Time{})
		return pc.conn, true, nil
	}
	conn, resp, err := p.dialer.DialContext(ctx, wsURL, p.header.Clone())
	if err != nil {
		if resp != nil {
			log.Printf("[connpool] dial failed oid=%s session=%s status=%d", oid, sessionID, resp.StatusCode)
		}
		return nil, false, err
	}
	return conn, false, nil
}

func (p *ConnPool) Return(oid, tid, sessionID string, conn *websocket.Conn) {
	if conn == nil {
		return
	}
	key := p.key(oid, tid, sessionID)
	pc := &pooledConn{conn: conn, created: time.Now()}
	p.mu.Lock()
	old := p.conns[key]
	p.conns[key] = pc
	p.mu.Unlock()
	if old != nil && old.conn != conn {
		_ = old.conn.Close()
	}
}

func (p *ConnPool) Discard(oid, tid, sessionID string, conn *websocket.Conn) {
	key := p.key(oid, tid, sessionID)
	p.mu.Lock()
	if pc := p.conns[key]; pc != nil && (conn == nil || pc.conn == conn) {
		delete(p.conns, key)
	}
	p.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (p *ConnPool) GC() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, pc := range p.conns {
		if now.Sub(pc.created) > 5*time.Minute {
			pc.conn.Close()
			delete(p.conns, k)
		}
	}
}

func (p *ConnPool) Close() {
	close(p.stop)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, pc := range p.conns {
		pc.conn.Close()
		delete(p.conns, k)
	}
}

func (p *ConnPool) Stats() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{"pooled_connections": len(p.conns)}
}

func (p *ConnPool) gcLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.GC()
		}
	}
}
