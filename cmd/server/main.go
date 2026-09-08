// Command server runs the aichatdeck Core Loop MVP: agent registration,
// discovery/manifest, the messaging envelope, and task claim/verify.
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"aichatdeck/internal/bus"
	"aichatdeck/internal/config"
	"aichatdeck/internal/db"
	"aichatdeck/internal/httpapi"
	"aichatdeck/internal/notify"
	"aichatdeck/internal/store"
)

func main() {
	cfg := config.Load()

	sqlDB, err := db.Open(cfg.SQLitePath)
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}
	defer sqlDB.Close()

	redisBus := bus.NewRedis(cfg.RedisAddr)
	defer redisBus.Close()

	if err := redisBus.Ping(context.Background()); err != nil {
		log.Fatalf("connect redis at %s: %v", cfg.RedisAddr, err)
	}

	server := &httpapi.Server{
		Agents:           store.NewAgentStore(sqlDB),
		Credentials:      store.NewCredentialStore(sqlDB),
		Tasks:            store.NewTaskStore(sqlDB),
		Messages:         store.NewMessageStore(sqlDB),
		Knowledge:        store.NewKnowledgeStore(sqlDB),
		Artifacts:        store.NewArtifactStore(sqlDB),
		Projects:         store.NewProjectStore(sqlDB),
		Members:          store.NewMembershipStore(sqlDB),
		OperatorRequests: store.NewOperatorRequestStore(sqlDB),
		Bus:              redisBus,
		DB:               sqlDB,
		RedisPinger:      redisBus,
		Logger:           log.Default(),
	}
	server.TelegramUserID = cfg.TelegramUserID
	server.TelegramWebhookSecret = cfg.TelegramWebhookSecret
	if cfg.TelegramBotToken != "" && cfg.TelegramChatID != "" {
		server.OperatorNotifier = &notify.Telegram{Token: cfg.TelegramBotToken, ChatID: cfg.TelegramChatID}
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewRouter(server),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	alertsDone := make(chan struct{})
	go func() { defer close(alertsDone); server.RunVerificationAlerts(ctx) }()

	go func() {
		log.Printf("aichatdeck listening on %s (sqlite=%s redis=%s)", cfg.Addr, cfg.SQLitePath, cfg.RedisAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	<-alertsDone
}
