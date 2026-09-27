package config

import (
	"os"
)

type Config struct {
	HTTPAddr      string
	DatabaseURL   string
	RedisAddr     string
	RedisDB       int
	KafkaBrokers  string
	ServerSecret  string
	AutoMigrate   bool
	Seed          bool
	RunWorkers    bool
	ComposePrefix string
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func Load() Config {
	return Config{
		HTTPAddr:     getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:  getenv("DATABASE_URL", "host=/workspace/162-b/.local/run port=5433 user=recycle dbname=recycling sslmode=disable"),
		RedisAddr:    getenv("REDIS_ADDR", "127.0.0.1:6379"),
		RedisDB:      15,
		KafkaBrokers: getenv("KAFKA_BROKERS", "127.0.0.1:9092"),
		ServerSecret: getenv("SERVER_SECRET", "recycle-demo-secret-2026"),
		AutoMigrate:  getenv("AUTO_MIGRATE", "true") != "false",
		Seed:         getenv("SEED", "true") != "false",
		RunWorkers:   getenv("RUN_WORKERS", "true") != "false",
	}
}
