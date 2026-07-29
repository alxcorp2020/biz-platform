// Command apiserver runs the public read API (spec 13.1) against Postgres.
// Deploy target: Railway / Render free tier. Listens on $PORT (platform
// convention) and reads the DB connection string from $DATABASE_URL.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"

	_ "github.com/lib/pq"

	"biz-platform/collector/internal/api"
	"biz-platform/collector/internal/migrate"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Error("DATABASE_URL is not set")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(10)

	if err := db.Ping(); err != nil {
		logger.Error("database ping failed", "error", err)
		os.Exit(1)
	}

	if err := migrate.Apply(context.Background(), db); err != nil {
		logger.Error("schema migration failed", "error", err)
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := api.New(db, logger)
	logger.Info("api server starting", "port", port)
	if err := http.ListenAndServe(":"+port, srv.Routes()); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
