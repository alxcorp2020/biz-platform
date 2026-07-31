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

	"github.com/anthropics/anthropic-sdk-go"
	_ "github.com/lib/pq"

	"biz-platform/collector/internal/api"
	"biz-platform/collector/internal/billing"
	"biz-platform/collector/internal/collector"
	"biz-platform/collector/internal/collector/pgstore"
	"biz-platform/collector/internal/collector/runner"
	"biz-platform/collector/internal/collector/sources/demo"
	"biz-platform/collector/internal/collector/sources/g2b"
	"biz-platform/collector/internal/migrate"
	"biz-platform/collector/internal/notify"
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

	attachmentDir := os.Getenv("ATTACHMENT_DIR")
	if attachmentDir == "" {
		attachmentDir = "./data/attachments"
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		logger.Warn("ANTHROPIC_API_KEY is not set; 면허·인증 증빙서류 업로드 AI 추출은 요청 시 실패합니다")
	}
	anthropicClient := anthropic.NewClient()

	resendFrom := os.Getenv("RESEND_FROM_EMAIL")
	if resendFrom == "" {
		resendFrom = "알림 <onboarding@resend.dev>"
	}
	notifyClient := notify.NewClient(os.Getenv("RESEND_API_KEY"), resendFrom)
	if !notifyClient.Configured() {
		logger.Warn("RESEND_API_KEY is not set; 이메일 알림(마감 리마인더/추천 다이제스트/담당자 알림)은 발송되지 않습니다")
	}

	smsNotifyClient := notify.NewSMSClient(os.Getenv("ALIGO_API_KEY"), os.Getenv("ALIGO_USER_ID"), os.Getenv("ALIGO_SENDER"))
	if !smsNotifyClient.Configured() {
		logger.Warn("ALIGO_API_KEY/ALIGO_USER_ID/ALIGO_SENDER is not set; SMS 알림(마감 리마인더/담당자 상태변경)은 발송되지 않습니다")
	}

	tossClient := billing.NewTossClient(os.Getenv("TOSS_SECRET_KEY"))
	if !tossClient.Configured() {
		logger.Warn("TOSS_SECRET_KEY is not set; 결제 승인(POST /api/billing/confirm)은 요청 시 실패합니다 (테스트 키 발급 전)")
	}
	tossClientKey := os.Getenv("TOSS_CLIENT_KEY")
	if tossClientKey == "" {
		logger.Warn("TOSS_CLIENT_KEY is not set; 결제위젯이 뜨지 않습니다 (테스트 키 발급 전)")
	}

	srv := api.New(db, logger, loadSessionSecret(logger), attachmentDir, &anthropicClient, notifyClient, smsNotifyClient, tossClient, tossClientKey)
	startBackgroundNotifications(srv, logger)

	logger.Info("api server starting", "port", port)
	if err := http.ListenAndServe(":"+port, srv.Routes()); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// dailyNotificationHour/Minute: 이메일 알림 배치(마감 D-3/D-1, 추천공고
// 다이제스트)를 매일 이 시각(KST)에 1회 실행한다. 특별한 근거가 있는
// 시각은 아니고 "업무 시작 직후 확인 가능하게"라는 상식적 기본값 —
// 필요하면 나중에 조정.
const (
	dailyNotificationHour   = 9
	dailyNotificationMinute = 0
)

// startBackgroundNotifications runs api.Server.RunDailyNotifications once a
// day, same in-process-goroutine workaround as startBackgroundCollection
// (Render 무료 플랜은 별도 Background Worker를 지원하지 않음).
func startBackgroundNotifications(srv *api.Server, logger *slog.Logger) {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		logger.Error("background notifications: failed to load Asia/Seoul timezone, notifications disabled", "error", err)
		return
	}
	go func() {
		ctx := context.Background()
		for {
			wait := time.Until(notify.NextDailyRun(time.Now(), loc, dailyNotificationHour, dailyNotificationMinute))
			time.Sleep(wait)
			// 자동전환을 알림보다 먼저 실행 — 오늘 막 자동 제외된 건이
			// 같은 배치에서 쓸모없는 마감 리마인더를 한 번 더 받지 않도록.
			if deadlinePassed, noticeClosed, err := srv.RunPipelineAutoTransitions(ctx); err != nil {
				logger.Error("pipeline auto-transition batch failed", "error", err)
			} else if deadlinePassed > 0 || noticeClosed > 0 {
				logger.Info("pipeline auto-transition batch completed", "deadlinePassed", deadlinePassed, "noticeClosed", noticeClosed)
			}
			srv.RunDailyNotifications(ctx)
		}
	}()
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
