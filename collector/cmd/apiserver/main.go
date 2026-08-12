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
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	_ "github.com/lib/pq"

	"biz-platform/collector/internal/api"
	"biz-platform/collector/internal/billing"
	"biz-platform/collector/internal/collector"
	"biz-platform/collector/internal/collector/pgstore"
	"biz-platform/collector/internal/collector/runner"
	"biz-platform/collector/internal/collector/sources/bizinfo"
	"biz-platform/collector/internal/collector/sources/demo"
	"biz-platform/collector/internal/collector/sources/g2b"
	"biz-platform/collector/internal/collector/sources/scsbid"
	"biz-platform/collector/internal/migrate"
	"biz-platform/collector/internal/notify"
	"biz-platform/collector/internal/oauth"
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

	appBaseURL := os.Getenv("APP_BASE_URL")
	if appBaseURL == "" {
		appBaseURL = "http://localhost:" + port
		logger.Warn("APP_BASE_URL is not set; 팀 초대 이메일 링크가 localhost를 가리키고, 소셜 로그인(구글/네이버/카카오) redirect_uri도 http://localhost.../api/auth/{provider}/callback로 만들어집니다 — 이 값이 각 제공자 콘솔에 등록된 리다이렉트 URI와 다르면 로그인 화면에서 '리다이렉트 URI 불일치' 에러가 납니다. 운영 배포 시 반드시 실제 공개 URL로 설정할 것")
	}

	googleRedirectURI := strings.TrimRight(appBaseURL, "/") + "/api/auth/google/callback"
	googleOAuth := oauth.NewGoogleClient(os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET"), googleRedirectURI)
	if !googleOAuth.Configured() {
		logger.Warn("GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET is not set; '구글로 로그인' 버튼은 요청 시 404를 반환합니다")
	} else {
		// "리다이렉트 URI 불일치" 에러 진단용 — Google Cloud Console의
		// 승인된 리디렉션 URI와 이 값이 한 글자라도 다르면 로그인이
		// 실패한다(대소문자/트레일링 슬래시/http vs https 전부 포함).
		logger.Info("google oauth configured", "redirectURI", googleRedirectURI)
	}
	naverRedirectURI := strings.TrimRight(appBaseURL, "/") + "/api/auth/naver/callback"
	naverOAuth := oauth.NewNaverClient(os.Getenv("NAVER_CLIENT_ID"), os.Getenv("NAVER_CLIENT_SECRET"), naverRedirectURI)
	if !naverOAuth.Configured() {
		logger.Warn("NAVER_CLIENT_ID/NAVER_CLIENT_SECRET is not set; '네이버로 로그인' 버튼은 요청 시 404를 반환합니다")
	} else {
		logger.Info("naver oauth configured", "redirectURI", naverRedirectURI)
	}
	kakaoRedirectURI := strings.TrimRight(appBaseURL, "/") + "/api/auth/kakao/callback"
	kakaoOAuth := oauth.NewKakaoClient(os.Getenv("KAKAO_REST_API_KEY"), os.Getenv("KAKAO_CLIENT_SECRET"), kakaoRedirectURI)
	if !kakaoOAuth.Configured() {
		logger.Warn("KAKAO_REST_API_KEY is not set; '카카오로 로그인' 버튼은 요청 시 404를 반환합니다")
	} else {
		logger.Info("kakao oauth configured", "redirectURI", kakaoRedirectURI)
	}

	scsbidSrc := newScsbidSource(logger)

	vapidPublicKey := os.Getenv("VAPID_PUBLIC_KEY")
	vapidPrivateKey := os.Getenv("VAPID_PRIVATE_KEY")
	vapidSubject := os.Getenv("VAPID_SUBJECT")
	if vapidPublicKey == "" || vapidPrivateKey == "" {
		logger.Warn("VAPID_PUBLIC_KEY/VAPID_PRIVATE_KEY is not set; 웹 푸시 알림(Phase 6)은 발송되지 않습니다")
	} else if vapidSubject == "" {
		vapidSubject = "mailto:admin@example.com"
		logger.Warn("VAPID_SUBJECT is not set; 기본값(mailto:admin@example.com)을 씁니다 — 운영 배포 시 실제 연락처로 설정 권장")
	}

	srv := api.New(db, logger, loadSessionSecret(logger), attachmentDir, &anthropicClient, notifyClient, smsNotifyClient, tossClient, tossClientKey, appBaseURL, scsbidSrc, vapidPublicKey, vapidPrivateKey, vapidSubject, googleOAuth, naverOAuth, kakaoOAuth)

	// srv가 만들어진 뒤에 수집 배치를 시작한다 — "정정된 관심공고" 즉시
	// 알림(2026-08-06)이 runner.Runner.OnChangesRecorded 콜백으로
	// srv.NotifyNoticeChanged를 호출해야 해서, 이 두 함수가 srv를 받는다.
	startBackgroundCollection(dsn, logger, srv)
	startBackgroundBizinfoCollection(dsn, logger, srv)
	startBackgroundNotifications(srv, logger, scsbidSrc)
	startBackgroundDeadlineSchedule(srv, logger)
	startBackgroundResultLookup(srv, logger)
	startBackgroundDocumentExtraction(srv, logger)
	startBackgroundNoticeEnrichment(srv, logger)

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
func startBackgroundNotifications(srv *api.Server, logger *slog.Logger, scsbidSrc *scsbid.Source) {
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
			if scsbidSrc != nil {
				if written, err := srv.RunAwardHistoryIngestion(ctx, scsbidSrc); err != nil {
					logger.Error("award history ingestion batch failed", "error", err)
				} else if written > 0 {
					logger.Info("award history ingestion batch completed", "recordsWritten", written)
				}
			}
			srv.RunDailyNotifications(ctx)
			// 매일 실행되지만 실제 리포트 생성은 월요일(주간)/1일(월간)에만
			// 일어난다 — RunScheduledReports 내부에서 날짜를 확인한다.
			if weekly, monthly, err := srv.RunScheduledReports(ctx, time.Now().In(loc)); err != nil {
				logger.Error("scheduled reports batch failed", "error", err)
			} else if weekly > 0 || monthly > 0 {
				logger.Info("scheduled reports batch completed", "weeklyGenerated", weekly, "monthlyGenerated", monthly)
			}
			if applied, err := srv.ApplyScheduledDowngrades(ctx); err != nil {
				logger.Error("scheduled downgrade batch failed", "error", err)
			} else if applied > 0 {
				logger.Info("scheduled downgrade batch completed", "applied", applied)
			}
			if applied, err := srv.ApplyScheduledCancellations(ctx); err != nil {
				logger.Error("scheduled cancellation batch failed", "error", err)
			} else if applied > 0 {
				logger.Info("scheduled cancellation batch completed", "applied", applied)
			}
		}
	}()
}

// startBackgroundDeadlineSchedule runs api.Server.RunDeadlineSchedule on a
// 30-minute ticker (Phase B+, 2026-08-09) — 참가자격/제출 마감의 시간단위
// 알림(H-6/H-2 포함)을 정확히 잡으려면 일일 배치로는 부족해서다. 30분 주기면
// H-2도 최대 30분 오차 안에서 발송된다. dedup은 DB(pipeline_deadline_events)
// 기준이라 서버 재시작·중복틱에도 같은 이벤트가 다시 나가지 않는다.
func startBackgroundDeadlineSchedule(srv *api.Server, logger *slog.Logger) {
	go func() {
		ctx := context.Background()
		runOnce := func() {
			if st, err := srv.RunDeadlineSchedule(ctx); err != nil {
				logger.Error("deadline schedule batch failed", "error", err)
			} else if st.Notifications > 0 || st.Changed > 0 {
				logger.Info("deadline schedule batch completed", "processed", st.Processed, "notifications", st.Notifications, "changed", st.Changed)
			}
		}
		runOnce()
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			runOnce()
		}
	}()
}

// startBackgroundResultLookup runs api.Server.RunResultLookup on a 30-minute
// ticker (우선순위5, 2026-08-09) — 개찰(opening_at) 이후 제출완료 건의 공식
// 낙찰 결과를 backoff(+30분/+2시간/+6시간/+24시간/+3일)로 자동 조회해 낙찰/탈락
// 자동전환하기 위함. G2B_SERVICE_KEY 미설정이면 RunResultLookup이 조용히 스킵
// (s.scsbidSource==nil). 조회/전환 dedup은 DB(result_finalized_at·
// result_check_attempts)로 관리해 재시작에도 안전하다.
func startBackgroundResultLookup(srv *api.Server, logger *slog.Logger) {
	go func() {
		ctx := context.Background()
		runOnce := func() {
			if st, err := srv.RunResultLookup(ctx); err != nil {
				logger.Error("result lookup batch failed", "error", err)
			} else if st.Processed > 0 {
				logger.Info("result lookup batch completed", "processed", st.Processed, "changed", st.Changed, "notifications", st.Notifications, "errors", st.Errors)
			}
		}
		runOnce()
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			runOnce()
		}
	}()
}

// startBackgroundNoticeEnrichment runs api.Server.RunNoticeEnrichment on a
// 15-minute ticker (Phase C, 2026-08-11) — 미보강 현재 버전에 참가가능지역/허용면허를
// 공식 오퍼레이션으로 증분 보강한다(1 사이클당 소량만 → 일일 쿼터 보호). G2B_SERVICE_KEY
// 미설정이면 보강 자체를 비활성화한다(다른 g2b 기능과 동일한 graceful degrade).
func startBackgroundNoticeEnrichment(srv *api.Server, logger *slog.Logger) {
	key := os.Getenv("G2B_SERVICE_KEY")
	if key == "" {
		logger.Warn("G2B_SERVICE_KEY is not set; notice enrichment(참가가능지역/허용면허) disabled")
		return
	}
	// 백필 가속 노브(환경변수, 미설정 시 기존과 동일한 보수값). g2b 실제 일일쿼터를 확인한
	// 뒤에만 상향할 것 — perDay를 안 올리고 batch/interval만 바꿔도 일일 상한은 그대로다.
	perSecond := envFloatDefault("NOTICE_ENRICHMENT_PER_SECOND", 1)
	dailyLimit := envIntDefault("NOTICE_ENRICHMENT_DAILY_LIMIT", 1000)
	intervalMin := envIntDefault("NOTICE_ENRICHMENT_INTERVAL_MINUTES", 15)
	if intervalMin < 1 {
		intervalMin = 15
	}
	enricher := g2b.NewEnrichmentClientWithLimits(key, perSecond, dailyLimit)
	// 상세 on-view 트리거도 같은 enricher(rate-limit/쿼터 공유)를 쓰게 한다 — 사용자가 연 공고를 우선 보강.
	srv.SetNoticeEnricher(enricher)
	logger.Info("notice enrichment configured",
		"perSecond", perSecond, "dailyLimit", dailyLimit, "intervalMin", intervalMin)
	go func() {
		ctx := context.Background()
		runOnce := func() {
			if n, err := srv.RunNoticeEnrichment(ctx, enricher); err != nil {
				logger.Error("notice enrichment batch failed", "error", err)
			} else if n > 0 {
				logger.Info("notice enrichment batch completed", "processed", n)
			}
		}
		runOnce()
		ticker := time.NewTicker(time.Duration(intervalMin) * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			runOnce()
		}
	}()
}

// envIntDefault/envFloatDefault — 환경변수 정수/실수 파싱(미설정·이상값이면 기본값).
func envIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloatDefault(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// startBackgroundDocumentExtraction runs api.Server.RunDocumentExtraction
// (Phase 4: 공고→제출서류/자격조건 추출 자동화, document_extraction.go)
// on the same 1-hour ticker as startBackgroundCollection — same in-process
// workaround, and it makes sense to follow shortly after each collection
// cycle since that's what feeds new attachments into the pipeline.
func startBackgroundDocumentExtraction(srv *api.Server, logger *slog.Logger) {
	go func() {
		ctx := context.Background()
		runOnce := func() {
			srv.RunDocumentExtraction(ctx)
		}
		runOnce()
		ticker := time.NewTicker(60 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			runOnce()
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
func startBackgroundCollection(dsn string, logger *slog.Logger, srv *api.Server) {
	go func() {
		ctx := context.Background()
		src, sourceName, baseURL := newCollectorSource(logger)
		st, err := pgstore.Open(ctx, dsn, src.SourceCode(), sourceName, "procurement", baseURL)
		if err != nil {
			logger.Error("background collection: failed to open store", "error", err)
			return
		}
		rn := runner.New(src, st, logger)
		rn.OnChangesRecorded = srv.NotifyNoticeChanged

		runOnce := func() {
			res := rn.RunIncremental(ctx, time.Time{})
			if res.Status == runner.StatusFailed {
				logger.Error("background collection cycle failed",
					"status", res.Status, "processed", res.ProcessedCount, "success", res.SuccessCount, "errors", res.Errors)
				return
			}
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

// startBackgroundBizinfoCollection mirrors startBackgroundCollection but runs
// a second, independent runner for bizinfo(기업마당/지자체 지원사업) —
// notices.notice_type='support_program', a completely separate stream from
// g2b's 'procurement' notices, so this is a second ticker+store rather than
// folding into the existing one. If BIZINFO_API_KEY isn't set, this
// batch is skipped entirely(no demo fallback — g2b/demo already provides
// baseline content, an empty support_program stream isn't broken).
func startBackgroundBizinfoCollection(dsn string, logger *slog.Logger, srv *api.Server) {
	key := os.Getenv("BIZINFO_API_KEY")
	if key == "" {
		logger.Warn("BIZINFO_API_KEY is not set; 기업마당/지자체 지원사업 수집이 비활성화됩니다")
		return
	}
	go func() {
		ctx := context.Background()
		src := bizinfo.New(key)
		// 운영 첫 수집을 통제하기 위한 선택적 상한(기본 미설정 = 기존 동작 유지).
		// BIZINFO_PAGE_SIZE=20 + BIZINFO_MAX_PAGES=1 → 한 주기당 20건만 수집(단계적
		// 운영 검증용). 값을 비우거나 유효하지 않으면 기존 기본값(PageSize=100,
		// MaxPages=1000)을 그대로 쓴다. 신규 기능이 아니라 수집량 안전장치일 뿐이다.
		if ps := positiveIntEnv("BIZINFO_PAGE_SIZE"); ps > 0 {
			src.PageSize = ps
		}
		st, err := pgstore.Open(ctx, dsn, src.SourceCode(), "중소벤처기업부_기업마당", "support_program", "https://www.bizinfo.go.kr/uss/rss/bizinfoApi.do")
		if err != nil {
			logger.Error("background bizinfo collection: failed to open store", "error", err)
			return
		}
		rn := runner.New(src, st, logger)
		if mp := positiveIntEnv("BIZINFO_MAX_PAGES"); mp > 0 {
			rn.MaxPages = mp
		}
		rn.OnChangesRecorded = srv.NotifyNoticeChanged
		logger.Info("background bizinfo collection configured",
			"page_size", src.PageSize, "max_pages", rn.MaxPages)

		runOnce := func() {
			res := rn.RunIncremental(ctx, time.Time{})
			if res.Status == runner.StatusFailed {
				// 실패 사유(예: bizinfo api error: 존재하지 않는 인증키)는 res.Errors에만
				// 담겨 기존엔 로그에 안 나왔다 — 원인 파악을 위해 반드시 함께 남긴다.
				logger.Error("background bizinfo collection cycle failed",
					"status", res.Status, "processed", res.ProcessedCount, "success", res.SuccessCount, "errors", res.Errors)
				return
			}
			logger.Info("background bizinfo collection cycle finished",
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

// positiveIntEnv reads an env var as a positive int, returning 0 when it's
// unset, non-numeric, or ≤0 — callers treat 0 as "keep the default".
func positiveIntEnv(name string) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
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

// newScsbidSource builds the 낙찰이력(notice_award_history) 수집기용
// scsbid.Source — G2B_SERVICE_KEY를 그대로 재사용한다(data.go.kr은 계정당
// 키 하나로 그 계정이 활용신청 승인받은 모든 API를 호출하므로, 별도
// SCSBID_SERVICE_KEY 환경변수를 추가할 필요가 없다). 키가 없으면 nil을
// 반환해 이 배치 자체를 건너뛴다(g2b처럼 데모 폴백은 없음 — 낙찰이력은
// 없어도 award_history.go가 count=0 정상 응답을 내려주므로 데모 데이터로
// 채울 이유가 없다).
func newScsbidSource(logger *slog.Logger) *scsbid.Source {
	key := os.Getenv("G2B_SERVICE_KEY")
	if key == "" {
		logger.Warn("G2B_SERVICE_KEY is not set; 낙찰이력(경쟁사/낙찰 히스토리) 수집이 비활성화됩니다")
		return nil
	}
	return scsbid.New(key)
}
