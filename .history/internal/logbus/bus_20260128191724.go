package logbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

type Event struct {
	TS            time.Time `json:"ts"`
	RequestID     string    `json:"request_id"`
	Facade        string    `json:"facade"`
	RequestModel  string    `json:"request_model"`
	UpstreamModel string    `json:"upstream_model"`
	ProviderType  string    `json:"provider_type"`
	PoolID        uint64    `json:"pool_id"`
	CredentialID  uint64    `json:"credential_id"`
	ClientKey     string    `json:"client_key"`
	Status        int       `json:"status"`
	LatencyMs     int64     `json:"latency_ms"`
	Error         string    `json:"error,omitempty"`
}

type Bus struct {
	db *sql.DB

	mu      sync.RWMutex
	subs    map[chan Event]struct{}
	ring    []Event
	ringCap int
}

func New(db *sql.DB, ringCap int) *Bus {
	if ringCap <= 0 {
		ringCap = 200
	}
	return &Bus{
		db:      db,
		subs:    make(map[chan Event]struct{}),
		ring:    make([]Event, 0, ringCap),
		ringCap: ringCap,
	}
}

func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	if len(b.ring) < b.ringCap {
		b.ring = append(b.ring, ev)
	} else {
		copy(b.ring, b.ring[1:])
		b.ring[len(b.ring)-1] = ev
	}
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	b.mu.Unlock()

	// Async persistence
	if b.db != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := b.db.ExecContext(ctx,
				`INSERT INTO request_logs (pool_id, provider_id, credential_id, client_key, facade, request_model, upstream_model, status_code, latency_ms, error_msg)
				 VALUES (?, (SELECT provider_id FROM credentials WHERE id=?), ?, ?, ?, ?, ?, ?, ?, ?)`,
				ev.PoolID, ev.CredentialID, ev.CredentialID, ev.ClientKey, ev.Facade, ev.RequestModel, ev.UpstreamModel, ev.Status, ev.LatencyMs, ev.Error)
			if err != nil {
				log.Printf("failed to persist log: %v", err)
			}
		}()
	}
}

func (b *Bus) ServeSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	snapshot := append([]Event(nil), b.ring...)
	b.mu.Unlock()

	for _, ev := range snapshot {
		writeSSE(w, ev)
	}
	flusher.Flush()

	defer func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case ev := <-ch:
			writeSSE(w, ev)
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, ev Event) {
	b, _ := json.Marshal(ev)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}
