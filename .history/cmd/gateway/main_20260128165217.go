package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"

	"claude-gateway/internal/admin"
	"claude-gateway/internal/config"
	"claude-gateway/internal/crypto"
	"claude-gateway/internal/db"
	"claude-gateway/internal/facade/anthropic"
	"claude-gateway/internal/facade/openai"
	"claude-gateway/internal/logbus"
	"claude-gateway/internal/metrics"
	"claude-gateway/internal/router"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	sqlDB, err := db.Open(cfg.MySQLDSN)
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	defer sqlDB.Close()

	if err := db.Migrate(sqlDB); err != nil {
		log.Fatalf("db migrate: %v", err)
	}

	m := metrics.New()
	cipher, err := crypto.NewAESGCMFromBase64Key(cfg.KeyEncMasterB64)
	if err != nil {
		log.Fatalf("cipher: %v", err)
	}
	rtr := router.New(sqlDB, m, cipher)
	bus := logbus.New(500)

	r := chi.NewRouter()
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSAllowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-API-Key", "Anthropic-Version"},
		ExposedHeaders:   []string{"Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	r.Mount("/metrics", m.Handler())

	v1 := chi.NewRouter()
	if cfg.ClientToken != "" {
		v1.Use(clientAuthMiddleware(cfg.ClientToken))
	}
	v1.Mount("/", anthropic.NewHandler(rtr, m, bus).Routes())
	v1.Mount("/", openai.NewHandler(rtr, m, bus).Routes())
	r.Mount("/v1", v1)

	r.Mount("/admin", admin.NewHandler(sqlDB, rtr, m, cipher, bus, cfg.AdminToken).Routes())

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func clientAuthMiddleware(token string) func(http.Handler) http.Handler {
	want := token
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimSpace(r.Header.Get("Authorization"))
			if strings.HasPrefix(got, "Bearer ") {
				got = strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
			} else {
				got = strings.TrimSpace(r.Header.Get("x-api-key"))
			}
			if got == "" {
				got = strings.TrimSpace(r.Header.Get("X-API-Key"))
			}
			if got != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
