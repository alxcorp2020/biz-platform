// Command collector-daemon periodically runs the collection pipeline against
// Postgres. On first deploy (no real government API key yet) it uses the
// bundled demo source so the API/frontend have real DB-backed data to show;
// swap DemoSource for a real sources/<code>.New(...) once an API key is issued.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"biz-platform/collector/internal/collector/pgstore"
	"biz-platform/collector/internal/collector/runner"
	"biz-platform/collector/internal/collector/sources/demo"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Error("DATABASE_URL is not set")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	src := demo.New()
	st, err := pgstore.Open(ctx, dsn, src.SourceCode(), "데모 데이터 소스", "procurement", "demo://local")
	if err != nil {
		logger.Error("failed to open pgstore", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	rn := runner.New(src, st, logger)

	intervalMin := 60
	logger.Info("collector-daemon starting", "interval_minutes", intervalMin)

	runOnce := func() {
		res := rn.RunIncremental(ctx, time.Time{})
		logger.Info("collection cycle finished",
			"status", res.Status, "processed", res.ProcessedCount,
			"success", res.SuccessCount, "failed", res.FailedCount)
	}

	runOnce() // 최초 1회는 즉시 실행 (기동 직후 API에 데이터가 비어있지 않도록)

	ticker := time.NewTicker(time.Duration(intervalMin) * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("collector-daemon shutting down")
			return
		case <-ticker.C:
			runOnce()
		}
	}
}
