package admin

import (
	"database/sql"
	"embed"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"claude-gateway/internal/crypto"
	"claude-gateway/internal/logbus"
	"claude-gateway/internal/metrics"
	"claude-gateway/internal/router"
)

//go:embed web/*
var webFS embed.FS

type Handler struct {
	db         *sql.DB
	rtr        *router.Router
	m          *metrics.Metrics
	cipher     *crypto.AESGCM
	bus        *logbus.Bus
	adminToken string
}

func NewHandler(db *sql.DB, rtr *router.Router, m *metrics.Metrics, cipher *crypto.AESGCM, bus *logbus.Bus, adminToken string) *Handler {
	return &Handler{db: db, rtr: rtr, m: m, cipher: cipher, bus: bus, adminToken: adminToken}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(h.authMiddleware)

	r.Get("/", h.serveIndex)
	r.Route("/api", func(r chi.Router) {
		r.Get("/providers", h.listProviders)
		r.Post("/providers", h.createProvider)
		r.Put("/providers/{id}", h.updateProvider)
		r.Delete("/providers/{id}", h.deleteProvider)

		r.Get("/credentials", h.listCredentials)
		r.Post("/credentials", h.createCredential)
		r.Put("/credentials/{id}", h.updateCredential)
		r.Delete("/credentials/{id}", h.deleteCredential)

		r.Get("/channels", h.listChannels)
		r.Post("/channels", h.createChannel)
		r.Put("/channels/{id}", h.updateChannel)
		r.Delete("/channels/{id}", h.deleteChannel)

		r.Get("/pools", h.listPools)
		r.Post("/pools", h.createPool)
		r.Put("/pools/{id}", h.updatePool)
		r.Delete("/pools/{id}", h.deletePool)

		r.Get("/rules", h.listRules)
		r.Post("/rules", h.createRule)
		r.Put("/rules/{id}", h.updateRule)
		r.Delete("/rules/{id}", h.deleteRule)

		r.Get("/logs/stream", h.logsStream)
	})
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return r
}

func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if strings.HasPrefix(got, "Bearer ") {
			got = strings.TrimPrefix(got, "Bearer ")
		}
		if strings.TrimSpace(got) == "" {
			got = r.Header.Get("X-Admin-Token")
		}

		if strings.TrimSpace(got) != h.adminToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "missing admin ui", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
