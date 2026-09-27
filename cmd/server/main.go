package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"recycle/internal/api"
	"recycle/internal/config"
	"recycle/internal/events"
	"recycle/internal/service"
	"recycle/internal/store"
)

func main() {
	cfg := config.Load()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// PostgreSQL
	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer db.Close()
	if cfg.AutoMigrate {
		if err := db.Migrate(ctx); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		log.Println("migrations applied")
	}

	// Redis（DB 15 与本机其他用途隔离）
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, DB: cfg.RedisDB})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("connect redis: %v", err)
	}

	svc := service.New(db.Pool, rdb)
	if cfg.Seed {
		if err := svc.Seed(ctx); err != nil {
			log.Fatalf("seed: %v", err)
		}
		log.Println("seed data ready")
	}
	if err := svc.SyncGeo(ctx); err != nil {
		log.Printf("sync recycler geo: %v", err)
	}

	brokers := []string{cfg.KafkaBrokers}
	relayStopped := make(chan struct{})
	workerStopped := make(chan struct{})

	if cfg.RunWorkers {
		// Kafka：主题、outbox relay、消费者组
		if err := events.EnsureTopics(brokers); err != nil {
			log.Fatalf("ensure kafka topics: %v", err)
		}
		relay, err := events.NewOutboxRelay(db.Pool, brokers)
		if err != nil {
			log.Fatalf("outbox relay: %v", err)
		}
		go func() {
			relay.Run(ctx, 500*time.Millisecond)
			_ = relay.Close()
			close(relayStopped)
		}()

		worker := events.NewWorker(db.Pool, svc)
		go func() {
			if err := worker.Run(ctx, brokers); err != nil {
				log.Printf("worker stopped: %v", err)
			}
			close(workerStopped)
		}()
		log.Println("kafka relay + workers started")
	} else {
		close(relayStopped)
		close(workerStopped)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.New(svc, cfg.ServerSecret).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("HTTP listening on %s", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, c := context.WithTimeout(context.Background(), 8*time.Second)
	defer c()
	_ = srv.Shutdown(shutdownCtx)
	<-relayStopped
	<-workerStopped
	_ = rdb.Close()
	log.Println("bye")
	os.Exit(0)
}
