package logbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	ProviderID    uint64    `json:"provider_id"`
	CredentialID  uint64    `json:"credential_id"`
	ClientKey     string    `json:"client_key"`
	SrcIP         string    `json:"src_ip,omitempty"`
	UserAgent     string    `json:"user_agent,omitempty"`
	IsTest        bool      `json:"is_test,omitempty"`
	Stream        bool      `json:"stream,omitempty"`
	RequestBytes  int       `json:"request_bytes,omitempty"`
	ResponseBytes int       `json:"response_bytes,omitempty"`
	InputTokens   int64     `json:"input_tokens,omitempty"`
	OutputTokens  int64     `json:"output_tokens,omitempty"`
	Status        int       `json:"status"`
	LatencyMs     int64     `json:"latency_ms"`
	TTFTMs        int64     `json:"ttft_ms,omitempty"`
	TPS           float64   `json:"tps,omitempty"`
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
				`INSERT INTO request_logs (request_id, pool_id, provider_id, credential_id, client_key, src_ip, user_agent, is_test, stream, request_bytes, response_bytes, facade, req_model, upstream_model, status, latency_ms, input_tokens, output_tokens, error_msg)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ev.RequestID, ev.PoolID, ev.ProviderID, ev.CredentialID, ev.ClientKey, ev.SrcIP, ev.UserAgent, ev.IsTest, ev.Stream, ev.RequestBytes, ev.ResponseBytes, ev.Facade, ev.RequestModel, ev.UpstreamModel, ev.Status, ev.LatencyMs, ev.InputTokens, ev.OutputTokens, ev.Error)
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
