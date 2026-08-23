package web

import (
	"log"
	"os"
	"sync"
	"time"
)

// persistStore 延迟磁盘持久化：内存变更只标记 dirty，由后台循环合并写盘，
// 避免高频路径在锁内做整文件写入。
type persistStore struct {
	writeMu    sync.Mutex
	dirtyMu    sync.Mutex
	dirty      bool
	flush      func() error
	registered bool
}

// Persist failure accounting: a failed session/conversation write must never
// look like a silent success that then breaks the next turn (session missing,
// conversation history lost). Every failure is logged at error level and
// counted so operators can see the store degrade in the version/health output.
var (
	persistFailMu    sync.Mutex
	persistFailCount int64
	persistLastErr   string
	persistLastAt    time.Time
)

func recordPersistFailure(storeName string, err error) {
	if err == nil {
		return
	}
	persistFailMu.Lock()
	persistFailCount++
	persistLastErr = storeName + ": " + err.Error()
	persistLastAt = time.Now()
	persistFailMu.Unlock()
	log.Printf("[persist] CRITICAL flush failed for %s: %v — the next request may lose session/conversation continuity", storeName, err)
}

// PersistFailureStats returns the accumulated flush-failure accounting for the
// version/health endpoints so a degraded data directory is visible.
func PersistFailureStats() map[string]any {
	persistFailMu.Lock()
	defer persistFailMu.Unlock()
	out := map[string]any{"count": persistFailCount, "last_error": persistLastErr}
	if !persistLastAt.IsZero() {
		out["last_at"] = persistLastAt
	}
	return out
}

func (p *persistStore) markDirty() {
	p.dirtyMu.Lock()
	p.dirty = true
	p.dirtyMu.Unlock()
	ensurePersistLoop(p)
}

func (p *persistStore) flushPending() {
	p.dirtyMu.Lock()
	if !p.dirty {
		p.dirtyMu.Unlock()
		return
	}
	p.dirty = false
	p.dirtyMu.Unlock()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.flush(); err != nil {
		p.dirtyMu.Lock()
		p.dirty = true
		p.dirtyMu.Unlock()
		recordPersistFailure("store", err)
	}
}

func (p *persistStore) flushNowBlocking() error {
	p.dirtyMu.Lock()
	p.dirty = false
	p.dirtyMu.Unlock()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.flush(); err != nil {
		p.dirtyMu.Lock()
		p.dirty = true
		p.dirtyMu.Unlock()
		recordPersistFailure("store", err)
		return err
	}
	return nil
}

var (
	persistMu      sync.Mutex
	persistList    []*persistStore
	persistSeen    map[*persistStore]bool
	persistOnce    sync.Once
	persistStop    chan struct{}
	persistStopped chan struct{}
)

// ensurePersistLoop registers a store exactly once. Repeated markDirty calls
// for the same store must not grow persistList, otherwise FlushAllPersist
// keeps doing increasingly redundant work on long-running processes.
func ensurePersistLoop(p *persistStore) {
	persistMu.Lock()
	if persistSeen == nil {
		persistSeen = map[*persistStore]bool{}
	}
	if !persistSeen[p] {
		persistSeen[p] = true
		p.registered = true
		persistList = append(persistList, p)
	}
	persistMu.Unlock()
	persistOnce.Do(func() {
		persistStop = make(chan struct{})
		persistStopped = make(chan struct{})
		go persistLoop()
	})
}

func persistLoop() {
	defer close(persistStopped)
	interval := 5 * time.Second
	if v := os.Getenv("M365_PERSIST_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 100*time.Millisecond {
			interval = d
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			FlushAllPersist()
		case <-persistStop:
			FlushAllPersist()
			return
		}
	}
}

// FlushAllPersist 同步落盘全部已注册的 store，供优雅停机调用。
func FlushAllPersist() {
	persistMu.Lock()
	list := append([]*persistStore(nil), persistList...)
	persistMu.Unlock()
	for _, p := range list {
		p.flushPending()
	}
}

var persistStopOnce sync.Once

// StopPersistLoop 停止后台循环并等待其完成最后一轮 flush。
func StopPersistLoop() {
	persistOnce.Do(func() {
		persistStop = make(chan struct{})
		persistStopped = make(chan struct{})
		go persistLoop()
	})
	persistStopOnce.Do(func() {
		close(persistStop)
	})
	<-persistStopped
}
