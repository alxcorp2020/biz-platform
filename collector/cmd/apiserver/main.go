// Command apiserver runs the public read API (spec 13.1) against Postgres.
// Deploy target: Railway / Render free tier. Listens on $PORT (platform
// convention) and reads the DB connection string from $DATABASE_URL.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"

	"biz-platform/collector/internal/api"
	"biz-platform/collector/internal/collector"
	"biz-platform/collector/internal/collector/pgstore"
	"biz-platform/collector/internal/collector/runner"
	"biz-platform/collector/internal/collector/sources/demo"
	"biz-platform/collector/internal/collector/sources/g2b"
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

	startBackgroundCollection(dsn, logger)

	srv := api.New(db, logger, loadSessionSecret(logger))
	logger.Info("api server starting", "port", port)
	if err := http.ListenAndServe(":"+port, srv.Routes()); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// loadSessionSecret reads SESSION_SECRET, or generates a random one if unset.
// A generated secret only lives for this process: it invalidates all
// sessions on restart and won't match across multiple instances, so set
// SESSION_SECRET explicitly for any real deployment.
func loadSessionSecret(logger *slog.Logger) []byte {
	if v := os.Getenv("SESSION_SECRET"); v != "" {
		return []byte(v)
	}
	logger.Warn("SESSION_SECRET is not set; generating a random secret for this process only " +
		"(sessions will be invalidated on restart and won't work across multiple instances)")
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		logger.Error("failed to generate random session secret", "error", err)
		os.Exit(1)
	}
	return secret
}

// startBackgroundCollection runs the collection pipeline inside the API
// server process on a 1-hour ticker. This exists because Render's free plan
// does not support a separate Background Worker service type — folding
// collection into the web service is the free-tier workaround. On a paid
// plan, prefer running cmd/collector-daemon as its own service instead and
// removing this call.
func startBackgroundCollection(dsn string, logger *slog.Logger) {
	go func() {
		ctx := context.Background()
		src, sourceName, baseURL := newCollectorSource(logger)
		st, err := pgstore.Open(ctx, dsn, src.SourceCode(), sourceName, "procurement", baseURL)
		if err != nil {
			logger.Error("background collection: failed to open store", "error", err)
			return
		}
		rn := runner.New(src, st, logger)

		runOnce := func() {
			res := rn.RunIncremental(ctx, time.Time{})
			logger.Info("background collection cycle finished",
				"status", res.Status, "processed", res.ProcessedCount, "success", res.SuccessCount)
		}

		runOnce()
		ticker := time.NewTicker(60 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			runOnce()
		}
	}()
}

// newCollectorSource picks the real 나라장터 source when G2B_SERVICE_KEY is
// configured, falling back to the bundled demo source otherwise so a fresh
// deploy without the key yet still has something to show.
func newCollectorSource(logger *slog.Logger) (collector.Collector, string, string) {
	if key := os.Getenv("G2B_SERVICE_KEY"); key != "" {
		return g2b.New(key), "조달청_나라장터 입찰공고정보서비스", "https://apis.data.go.kr/1230000/ad/BidPublicInfoService"
	}
	logger.Warn("G2B_SERVICE_KEY is not set; falling back to the bundled demo data source")
	return demo.New(), "데모 데이터 소스", "demo://local"
}
