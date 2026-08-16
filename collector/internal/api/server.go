// Package api exposes the read-only public endpoints from spec 13.1
// (GET /api/notices, GET /api/notices/{id}) directly against Postgres.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/lib/pq"

	"biz-platform/collector/internal/billing"
	"biz-platform/collector/internal/collector/sources/scsbid"
	"biz-platform/collector/internal/notify"
	"biz-platform/collector/internal/oauth"
	"biz-platform/collector/internal/webui"
)

// tossPaymentClient — *billing.TossClient의 실제 사용 부분만 뗀 인터페이스.
// 프로덕션에서는 항상 *billing.TossClient가 이 자리를 채우지만(New의
// tossClient 파라미터 타입은 그대로 *billing.TossClient — 호출부 변경
// 없음, 암묵적 인터페이스 충족), 결제 관련 테스트(billing_refund_test.go
// 등)에서 실제 Toss 서버에 네트워크 요청을 보내지 않는 가짜 구현으로
// 바꿔 끼울 수 있게 하려고 도입했다 — 2026-08-06 상위 플랜 환불 사고
// 수정을 로컬에서 재현 검증하면서 필요해짐.
type tossPaymentClient interface {
	Configured() bool
	Confirm(ctx context.Context, paymentKey, orderID string, amount int64) (*billing.ConfirmResult, []byte, error)
	Cancel(ctx context.Context, paymentKey, cancelReason string) (*billing.CancelResult, []byte, error)
}

type Server struct {
	db              *sql.DB
	logger          *slog.Logger
	sessionSecret   []byte
	attachmentDir   string
	anthropicClient *anthropic.Client
	notify          *notify.Client
	smsNotify       *notify.SMSClient
	toss            tossPaymentClient
	tossClientKey   string
	// appBaseURL — 팀 초대 이메일 링크 생성에만 쓰인다(company_team.go).
	// 프론트의 다른 리다이렉트(Toss 성공/실패 URL 등)는 location.origin을
	// 클라이언트에서 직접 쓰므로 이 값이 필요 없다 — 서버가 직접 링크
	// 문자열을 만들어야 하는 유일한 경우가 이메일 발송이라 여기만 필요.
	appBaseURL string
	// scsbidSource — nil이면(서비스키 미설정) 낙찰이력 수집이 비활성화된
	// 상태. handleRunAwardHistoryIngestion의 수동 트리거와 cmd/apiserver의
	// 일일 티커 둘 다 이 필드를 쓴다.
	scsbidSource *scsbid.Source
	// Phase 6: 웹 푸시(VAPID). vapidPrivateKey가 비어있으면 push_notifications.go의
	// 발송 함수들이 조용히 스킵한다(다른 채널의 "미설정 시 실패 로그만" 패턴과
	// 동일하게, 서버가 죽지 않고 인앱 알림함/이메일 등 다른 채널은 그대로 동작).
	vapidPublicKey  string
	vapidPrivateKey string
	vapidSubject    string
	// oauthProviders — 간편로그인(구글/네이버/카카오). "google"/"naver"/"kakao"
	// 키로 항상 3개 다 채워져 있다(클라이언트ID 미설정이면 그 Client의
	// Configured()가 false일 뿐 — oauth_login.go의 핸들러가 이 맵에서
	// r.PathValue("provider")로 찾아 바로 404/동작 여부를 판단한다).
	oauthProviders map[string]oauth.Client
	// noticeEnricher — 상세 조회 시 미보강 공고(procurement)를 즉시 비동기 보강하는 on-demand
	// 트리거용. 배경 sweep(startBackgroundNoticeEnrichment)과 같은 EnrichmentClient를 공유해
	// rate-limit/일일쿼터를 함께 지킨다. nil(키 미설정)이면 on-view 트리거는 no-op.
	// enrichInflight — 같은 공고를 동시에 중복 보강하지 않도록 진행 중 notice_id를 표시(뮤텍스 보호).
	noticeEnricher   noticeEnricher
	enrichInflight   map[string]bool
	enrichInflightMu sync.Mutex
}

// SetNoticeEnricher — 배경 보강 티커와 동일한 enricher를 상세 on-view 트리거에도 공유시킨다
// (cmd/apiserver에서 호출). 미호출이면 on-view 보강은 비활성.
func (s *Server) SetNoticeEnricher(e noticeEnricher) { s.noticeEnricher = e }

func New(db *sql.DB, logger *slog.Logger, sessionSecret []byte, attachmentDir string, anthropicClient *anthropic.Client, notifyClient *notify.Client, smsNotifyClient *notify.SMSClient, tossClient *billing.TossClient, tossClientKey string, appBaseURL string, scsbidSource *scsbid.Source, vapidPublicKey, vapidPrivateKey, vapidSubject string, googleOAuth, naverOAuth, kakaoOAuth oauth.Client) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	// tossClient(*billing.TossClient)가 nil로 들어오면 그대로 인터페이스
	// 필드에 대입하지 않는다 — 인터페이스에 담긴 "타입 있는 nil 포인터"는
	// 그 자체로 nil이 아닌 값이 되어(Go의 흔한 함정) 기존 `s.toss == nil`
	// 가드가 더 이상 true가 안 되고, 뒤이은 s.toss.Configured() 호출이
	// nil 포인터 역참조로 패닉난다. 명시적으로 걸러서 진짜 nil 인터페이스로
	// 남겨야 그 가드가 계속 정상 동작한다.
	var toss tossPaymentClient
	if tossClient != nil {
		toss = tossClient
	}
	return &Server{
		db:              db,
		logger:          logger,
		sessionSecret:   sessionSecret,
		attachmentDir:   attachmentDir,
		anthropicClient: anthropicClient,
		notify:          notifyClient,
		smsNotify:       smsNotifyClient,
		toss:            toss,
		tossClientKey:   tossClientKey,
		appBaseURL:      appBaseURL,
		scsbidSource:    scsbidSource,
		vapidPublicKey:  vapidPublicKey,
		vapidPrivateKey: vapidPrivateKey,
		vapidSubject:    vapidSubject,
		oauthProviders:  newOAuthProviders(googleOAuth, naverOAuth, kakaoOAuth),
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/banners", s.handleListBanners)
	mux.HandleFunc("GET /api/notices", s.handleListNotices)
	mux.HandleFunc("GET /api/notices/suggest", s.handleNoticeSuggest)
	mux.HandleFunc("GET /api/industry-taxonomy", s.handleGetIndustryTaxonomy)
	mux.HandleFunc("GET /api/notices/{id}", s.handleGetNotice)
	mux.HandleFunc("GET /api/notices/{id}/opening-result", s.handleGetNoticeOpeningResult)
	mux.HandleFunc("GET /api/attachments/{id}/preview", s.handleAttachmentPreview)
	mux.HandleFunc("POST /api/auth/signup", s.handleSignup)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/phone/send-code", s.handleSendPhoneVerificationCode)
	mux.HandleFunc("POST /api/auth/phone/verify-code", s.handleVerifyPhoneCode)
	mux.HandleFunc("POST /api/auth/find-email", s.handleFindEmail)
	mux.HandleFunc("POST /api/auth/reset-password-request", s.handleResetPasswordRequest)
	mux.HandleFunc("POST /api/auth/reset-password", s.handleResetPassword)
	mux.HandleFunc("GET /api/auth/config", s.handleGetAuthConfig)
	mux.HandleFunc("POST /api/auth/verify-email", s.handleVerifyEmail)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/{provider}/start", s.handleOAuthStart)
	mux.HandleFunc("GET /api/auth/{provider}/callback", s.handleOAuthCallback)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/me/account", s.handleGetAccountSettings)
	mux.HandleFunc("PATCH /api/me/account", s.handleUpdateAccountPhone)
	mux.HandleFunc("POST /api/me/account/change-password", s.handleChangePassword)
	mux.HandleFunc("DELETE /api/me/account/oauth/{provider}", s.handleDisconnectOAuthProvider)
	mux.HandleFunc("POST /api/me/account/deactivate", s.handleSelfDeactivateAccount)
	mux.HandleFunc("PUT /api/me/company-profile", s.handleUpsertCompanyProfile)
	mux.HandleFunc("POST /api/me/business-registration/extract", s.handleExtractBusinessRegistration)
	mux.HandleFunc("POST /api/me/signup-agreement", s.handleSignupAgreement)
	mux.HandleFunc("POST /api/me/resend-verification-email", s.handleResendVerificationEmail)
	mux.HandleFunc("GET /api/me/company/members", s.handleListCompanyMembers)
	mux.HandleFunc("DELETE /api/me/company/members/{id}", s.handleRemoveCompanyMember)
	mux.HandleFunc("POST /api/me/company/invitations", s.handleCreateInvitation)
	mux.HandleFunc("GET /api/invitations/{token}", s.handleGetInvitation)
	mux.HandleFunc("POST /api/invitations/{token}/accept", s.handleAcceptInvitation)
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("POST /api/notices/{id}/evaluate", s.handleEvaluateNotice)
	mux.HandleFunc("PUT /api/notices/{noticeId}/documents/{documentId}/checklist", s.handleToggleChecklistItem)
	mux.HandleFunc("GET /api/review/queue", s.handleReviewQueue)
	mux.HandleFunc("POST /api/review/eligibility-conditions/{id}", s.handleReviewEligibilityCondition)
	mux.HandleFunc("POST /api/review/required-documents/{id}", s.handleReviewRequiredDocument)
	mux.HandleFunc("POST /api/me/company-profile/documents", s.handleUploadCompanyDocument)
	mux.HandleFunc("POST /api/me/licenses", s.handleCreateLicense)
	mux.HandleFunc("GET /api/me/licenses", s.handleListLicenses)
	mux.HandleFunc("POST /api/me/certifications", s.handleCreateCertification)
	mux.HandleFunc("GET /api/me/certifications", s.handleListCertifications)
	mux.HandleFunc("POST /api/me/direct-production", s.handleSetDirectProductionStatus)
	mux.HandleFunc("POST /api/me/financials/documents", s.handleUploadFinancialDocument)
	mux.HandleFunc("POST /api/me/financials", s.handleCreateFinancial)
	mux.HandleFunc("GET /api/me/financials", s.handleListFinancials)
	mux.HandleFunc("POST /api/me/track-records/documents", s.handleUploadTrackRecordDocument)
	mux.HandleFunc("POST /api/me/track-records", s.handleCreateTrackRecord)
	mux.HandleFunc("GET /api/me/track-records", s.handleListTrackRecords)
	mux.HandleFunc("POST /api/me/personnel/documents", s.handleUploadPersonnelDocument)
	mux.HandleFunc("POST /api/me/personnel", s.handleCreatePersonnel)
	mux.HandleFunc("GET /api/me/personnel", s.handleListPersonnel)
	mux.HandleFunc("POST /api/me/intellectual-property/documents", s.handleUploadIPDocument)
	mux.HandleFunc("POST /api/me/intellectual-property", s.handleCreateIP)
	mux.HandleFunc("GET /api/me/intellectual-property", s.handleListIP)
	mux.HandleFunc("GET /api/me/profile-completeness", s.handleGetProfileCompleteness)
	mux.HandleFunc("POST /api/me/onboarding/complete", s.handleCompleteOnboarding)
	mux.HandleFunc("GET /api/me/contacts", s.handleListContacts)
	mux.HandleFunc("POST /api/me/contacts", s.handleCreateContact)
	mux.HandleFunc("PATCH /api/me/contacts/{id}", s.handleUpdateContact)
	mux.HandleFunc("DELETE /api/me/contacts/{id}", s.handleDeleteContact)

	mux.HandleFunc("GET /api/me/saved-searches", s.handleListSavedSearches)
	mux.HandleFunc("POST /api/me/saved-searches", s.handleCreateSavedSearch)
	mux.HandleFunc("PATCH /api/me/saved-searches/{id}", s.handleUpdateSavedSearch)
	mux.HandleFunc("DELETE /api/me/saved-searches/{id}", s.handleDeleteSavedSearch)
	mux.HandleFunc("POST /api/me/saved-searches/{id}/duplicate", s.handleDuplicateSavedSearch)
	mux.HandleFunc("PUT /api/me/saved-searches/{id}/active", s.handleSetSavedSearchActive)
	mux.HandleFunc("POST /api/me/company-profile/employee-verification/documents", s.handleUploadEmployeeVerificationDocument)
	mux.HandleFunc("POST /api/me/company-profile/employee-verification", s.handleConfirmEmployeeVerification)
	mux.HandleFunc("POST /api/notices/{id}/pipeline", s.handleCreatePipelineEntry)
	mux.HandleFunc("POST /api/notices/{id}/exclude", s.handleExcludeNotice)
	mux.HandleFunc("POST /api/notices/{id}/share", s.handleShareNotice)
	mux.HandleFunc("PATCH /api/pipeline/{id}", s.handleUpdatePipelineEntry)
	mux.HandleFunc("DELETE /api/pipeline/{id}", s.handleDeletePipelineEntry)
	mux.HandleFunc("PATCH /api/pipeline/{id}/checklist/{itemId}", s.handleUpdateChecklistItem)
	mux.HandleFunc("GET /api/pipeline", s.handleListPipeline)
	mux.HandleFunc("GET /api/pipeline/{id}", s.handleGetPipelineEntry)
	mux.HandleFunc("GET /api/pipeline/{id}/calendar.ics", s.handleGetPipelineCalendar)
	mux.HandleFunc("GET /api/pipeline/calendar-events", s.handleListPipelineCalendarEvents)
	mux.HandleFunc("PATCH /api/me/notification-settings", s.handleUpdateNotificationSettings)
	mux.HandleFunc("GET /api/me/notifications", s.handleListInAppNotifications)
	mux.HandleFunc("GET /api/me/notifications/unread-count", s.handleUnreadNotificationCount)
	mux.HandleFunc("POST /api/me/notifications/read-all", s.handleMarkAllNotificationsRead)
	mux.HandleFunc("POST /api/me/notifications/{id}/read", s.handleMarkNotificationRead)
	mux.HandleFunc("GET /api/push/vapid-public-key", s.handleGetPushPublicKey)
	mux.HandleFunc("POST /api/me/push-subscriptions", s.handleSubscribePush)
	mux.HandleFunc("DELETE /api/me/push-subscriptions", s.handleUnsubscribePush)
	mux.HandleFunc("POST /api/admin/push/test", s.handleAdminTestPush)
	mux.HandleFunc("POST /api/admin/run-notifications", s.handleRunNotifications)
	mux.HandleFunc("POST /api/admin/run-pipeline-auto-transitions", s.handleRunPipelineAutoTransitions)
	mux.HandleFunc("POST /api/admin/run-award-history-ingestion", s.handleRunAwardHistoryIngestion)
	mux.HandleFunc("POST /api/admin/run-deadline-schedule", s.handleRunDeadlineSchedule)
	mux.HandleFunc("POST /api/admin/run-result-lookup", s.handleRunResultLookup)
	mux.HandleFunc("POST /api/admin/run-notice-datetime-backfill", s.handleRunNoticeDatetimeBackfill)
	mux.HandleFunc("POST /api/admin/run-procurement-class-backfill", s.handleRunProcurementClassBackfill)
	mux.HandleFunc("POST /api/admin/run-document-extraction", s.handleRunDocumentExtraction)
	mux.HandleFunc("GET /api/me/subscription", s.handleGetSubscription)
	mux.HandleFunc("GET /api/me/payment-history", s.handleGetPaymentHistory)
	mux.HandleFunc("GET /api/reports", s.handleListReports)
	mux.HandleFunc("GET /api/growth-analytics", s.handleGetGrowthAnalytics)
	mux.HandleFunc("GET /api/me/ai-usage", s.handleGetAIUsage)
	mux.HandleFunc("POST /api/me/documents/{id}/retry", s.handleRetryDocumentExtraction)
	mux.HandleFunc("GET /api/billing/config", s.handleGetBillingConfig)
	mux.HandleFunc("POST /api/billing/checkout", s.handleBillingCheckout)
	mux.HandleFunc("POST /api/billing/confirm", s.handleBillingConfirm)
	mux.HandleFunc("POST /api/billing/cancel-downgrade", s.handleCancelDowngrade)
	mux.HandleFunc("POST /api/billing/refund-request", s.handleBillingRefundRequest)
	mux.HandleFunc("POST /api/billing/cancel-renewal", s.handleCancelRenewal)
	mux.HandleFunc("POST /api/billing/resume-renewal", s.handleResumeRenewal)
	mux.HandleFunc("POST /api/admin/run-scheduled-downgrades", s.handleRunScheduledDowngrades)
	mux.HandleFunc("POST /api/admin/run-scheduled-cancellations", s.handleRunScheduledCancellations)
	mux.HandleFunc("GET /api/admin/dashboard", s.handleAdminDashboard)
	mux.HandleFunc("GET /api/admin/members", s.handleAdminListMembers)
	mux.HandleFunc("GET /api/admin/members/{id}", s.handleAdminGetMember)
	mux.HandleFunc("POST /api/admin/members/{id}/deactivate", s.handleAdminDeactivateMember)
	mux.HandleFunc("DELETE /api/admin/members/{id}", s.handleAdminDeleteMember)
	mux.HandleFunc("PUT /api/admin/members/{id}/ai-limit", s.handleAdminSetAIAnalysisLimit)
	mux.HandleFunc("GET /api/admin/changelog", s.handleAdminChangelog)
	mux.HandleFunc("GET /api/admin/tech-spec", s.handleAdminTechSpec)
	mux.HandleFunc("GET /api/admin/integrations", s.handleAdminIntegrations)
	mux.HandleFunc("GET /api/admin/industry-taxonomy", s.handleAdminListIndustryTaxonomy)
	mux.HandleFunc("POST /api/admin/industry-taxonomy", s.handleAdminCreateIndustryTaxonomy)
	mux.HandleFunc("PATCH /api/admin/industry-taxonomy/{id}", s.handleAdminUpdateIndustryTaxonomy)
	mux.HandleFunc("GET /api/admin/notification-failures", s.handleAdminNotificationFailures)
	mux.HandleFunc("GET /api/admin/automation-history", s.handleAdminAutomationHistory)
	mux.HandleFunc("GET /api/admin/settings", s.handleAdminGetSettings)
	mux.HandleFunc("PUT /api/admin/settings/free-plan-email-limit", s.handleAdminSetFreePlanEmailLimit)
	mux.HandleFunc("PUT /api/admin/settings/phone-verification-required", s.handleAdminSetPhoneVerificationRequired)
	mux.HandleFunc("GET /api/admin/plan-settings", s.handleAdminGetPlanSettings)
	mux.HandleFunc("PUT /api/admin/plan-settings", s.handleAdminUpdatePlanSettings)

	// 관리자 CMS 3~6번(배너/브로드캐스트/팝업/공지 게시판)
	mux.HandleFunc("POST /api/admin/cms-images", s.handleUploadCmsImage)
	mux.HandleFunc("GET /cms-images/{filename}", s.handleServeCmsImage)
	mux.HandleFunc("GET /api/admin/banners", s.handleAdminListBanners)
	mux.HandleFunc("POST /api/admin/banners", s.handleAdminCreateBanner)
	mux.HandleFunc("PATCH /api/admin/banners/{id}", s.handleAdminUpdateBanner)
	mux.HandleFunc("DELETE /api/admin/banners/{id}", s.handleAdminDeleteBanner)
	mux.HandleFunc("POST /api/admin/banners/{id}/move", s.handleAdminMoveBanner)
	mux.HandleFunc("POST /api/admin/broadcasts", s.handleCreateBroadcast)
	mux.HandleFunc("GET /api/admin/broadcasts", s.handleListBroadcasts)
	mux.HandleFunc("GET /api/popups", s.handleListPopups)
	mux.HandleFunc("GET /api/admin/popups", s.handleAdminListPopups)
	mux.HandleFunc("POST /api/admin/popups", s.handleAdminCreatePopup)
	mux.HandleFunc("PATCH /api/admin/popups/{id}", s.handleAdminUpdatePopup)
	mux.HandleFunc("DELETE /api/admin/popups/{id}", s.handleAdminDeletePopup)
	mux.HandleFunc("GET /api/announcements", s.handleListAnnouncements)
	mux.HandleFunc("GET /api/announcements/{id}", s.handleGetAnnouncement)
	mux.HandleFunc("POST /api/admin/announcements", s.handleAdminCreateAnnouncement)
	mux.HandleFunc("PATCH /api/admin/announcements/{id}", s.handleAdminUpdateAnnouncement)
	mux.HandleFunc("DELETE /api/admin/announcements/{id}", s.handleAdminDeleteAnnouncement)
	mux.HandleFunc("GET /api/company-info", s.handleGetCompanyInfo)
	mux.HandleFunc("GET /api/legal-documents/{type}", s.handleGetLegalDocument)
	mux.HandleFunc("GET /api/admin/legal-documents", s.handleAdminListLegalDocuments)
	mux.HandleFunc("POST /api/admin/legal-documents", s.handleAdminPublishLegalDocument)
	mux.HandleFunc("PUT /api/admin/company-info", s.handleAdminUpdateCompanyInfo)
	// 정적 파일(webui.Handler(), "/" 캐치올)보다 이 정확 경로가 우선
	// 매칭된다(Go 1.22+ ServeMux 규칙) — static/manifest.json 파일은 이제
	// 안 쓰임(브랜드명이 바뀔 때마다 즉시 반영되도록 매 요청마다 DB에서
	// 새로 만듦).
	mux.HandleFunc("GET /manifest.json", s.handleManifest)

	mux.Handle("/", webui.Handler())
	return withLogging(s.logger, withCORS(mux))
}

type noticeListItem struct {
	ID               string     `json:"id"`
	NoticeType       string     `json:"noticeType"` // "procurement" | "support_program"
	Title            string     `json:"title"`
	OrganizationName string     `json:"organizationName"`
	Region           string     `json:"region"`
	Industry         string     `json:"industry"`
	Status           string     `json:"status"`
	ApplicationEndAt *time.Time `json:"applicationEndAt"`
	BudgetAmount     *int64     `json:"budgetAmount"`
	OfficialURL      string     `json:"officialUrl"`
	CurrentVersion   int        `json:"currentVersion"`
	IsBookmarked     bool       `json:"isBookmarked"`
	// PublishedAt/FirstCollectedAt/LastVerifiedAt/SourceName — 2026-08-06
	// 데이터 신뢰성 노출. 상세 조회(handleGetNotice)에서만 채운다(목록은
	// 이 값들을 안 씀) — 상세 화면에 "공고 게시/플랫폼 수집/마지막 확인/
	// 출처"를 표시해 사용자가 데이터 최신성을 판단할 수 있게 한다.
	PublishedAt      *time.Time `json:"publishedAt,omitempty"`
	FirstCollectedAt *time.Time `json:"firstCollectedAt,omitempty"`
	LastVerifiedAt   *time.Time `json:"lastVerifiedAt,omitempty"`
	SourceName       string     `json:"sourceName,omitempty"`
	// RegionRestricted/RecentlyChanged — 2026-08-06 정정 가시성 개선.
	// RegionRestricted는 nil이면 "정보 없음"(값을 안 주는 소스), true/false면
	// 실제 판단값. RecentlyChanged는 목록 조회에서만 계산한다(최근 7일 내
	// notice_changes 존재 여부).
	RegionRestricted *bool `json:"regionRestricted,omitempty"`
	RecentlyChanged  bool  `json:"recentlyChanged,omitempty"`
	// MatchReasons — 2026-08-06, 맞춤공고 "결과 보기"에서 "왜 이 공고가
	// 뽑혔는지"를 보여주기 위한 필드. handleListNotices가 이 요청에 실제로
	// 걸려 있던 필터(지역/업종/발주기관/예산범위/포함키워드)를 근거로
	// 계산해서 채운다 — 일반 검색(#/notices 수동 검색)에서도 값은 채워지지만
	// 프론트는 맞춤공고 결과 화면(savedSearchName 파라미터가 있을 때)에서만
	// 배지로 그린다(평소 검색 화면은 원칙대로 단순하게 유지).
	MatchReasons []string `json:"matchReasons,omitempty"`
	// Grade/GradeReason — 2026-08-06, "추천공고 왜 목록단계 노출"(브랜딩/UX
	// 현황점검 4번). 상세 페이지(AI 참여분석 탭)에서만 보이던 등급판정을
	// 검색 목록에서도 바로 보여준다 — attachNoticeGrades가 로그인 + 회사
	// 프로필이 있을 때만 채운다(company_pipeline.go의 attachPipelineGrades와
	// 동일하게 영속 컬럼이 아니라 매 요청 scoreNoticeForCompany로 재계산).
	Grade       string `json:"grade,omitempty"`
	GradeReason string `json:"gradeReason,omitempty"`
	// JointVentureRecommended/JointVentureReason — 2026-08-07, 실적 규모
	// 신뢰도 서브태그(Grade와 독립, scoring.go의 participationScore 주석
	// 참고). attachNoticeGrades가 Grade/GradeReason과 함께 채운다.
	JointVentureRecommended bool   `json:"jointVentureRecommended,omitempty"`
	JointVentureReason      string `json:"jointVentureReason,omitempty"`
	// PipelineEntryId/PipelineStatus — 2026-08-08. 이 공고가 로그인 사용자 회사의
	// 파이프라인(진행 중 사업)에 이미 담겨 있는지. 목록의 "참여검토" 버튼이
	// 새로고침 후에도 상태를 반영하고(담김↔참여검토 토글) 취소까지 하게 하려는
	// 것. 담긴 항목이 없으면 둘 다 빈 문자열(비로그인/프로필 없음 포함).
	PipelineEntryId string `json:"pipelineEntryId,omitempty"`
	PipelineStatus  string `json:"pipelineStatus,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db_unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

const (
	defaultNoticeListLimit = 20
	maxNoticeListLimit     = 100
)

// parseListingIntParam parses an offset/limit query param, falling back to
// def on empty/invalid/negative input — callers never see a malformed page
// as a 400, they just get sane defaults (같은 관용: 목록 조회는 사용자 입력
// 실수로 에러 화면을 띄우기보다 조용히 기본값으로 복구하는 쪽이 낫다).
func parseListingIntParam(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// splitNonEmpty splits a comma-separated string and trims/drops empty
// pieces — shared by the keywordsInclude WHERE절 빌더와 matchReasons의
// 키워드 하이라이트 로직이 "무엇을 하나의 키워드로 칠지"에 대해 항상
// 같은 정의를 쓰게 한다.
func splitNonEmpty(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// computeBaseMatchReasons — 2026-08-06, 맞춤공고 "결과 보기"에서 "왜 이
// 공고가 뽑혔는지" 표시용(비드큐 P1 9번). 지역/업종/발주기관/예산범위는
// handleListNotices의 WHERE절에서 전부 AND 조건으로 걸리므로, 이 함수가
// 반환하는 목록에 포함된 이상 그 쿼리 결과 안의 모든 행이 이미 만족한
// 상태다 — 그래서 row별 값을 다시 비교할 필요 없이 "이 필터가 걸려
// 있었는지"만 보면 된다(포함 키워드는 OR 조건이라 다르다 —
// computeKeywordMatchReason 참고).
func computeBaseMatchReasons(region, industry, organizationName, budgetMinRaw, budgetMaxRaw string) []string {
	var reasons []string
	var locFields []string
	if region != "" {
		locFields = append(locFields, "지역")
	}
	if industry != "" {
		locFields = append(locFields, "업종")
	}
	if len(locFields) > 0 {
		reasons = append(reasons, strings.Join(locFields, "·")+" 조건 일치")
	}
	if organizationName != "" {
		reasons = append(reasons, "발주기관 조건 일치")
	}
	if budgetMinRaw != "" || budgetMaxRaw != "" {
		reasons = append(reasons, "예산범위 조건 일치")
	}
	return reasons
}

// computeKeywordMatchReason returns which of matchKeywords actually appear
// in title(대소문자 무시 부분일치 — SQL ILIKE와 같은 기준) — 포함 키워드는
// OR 조건이라 다른 필터들과 달리 설정된 키워드 전부가 이 특정 행에
// 걸렸다고 보장되지 않는다.
func computeKeywordMatchReason(matchKeywords []string, title string) string {
	if len(matchKeywords) == 0 {
		return ""
	}
	lowerTitle := strings.ToLower(title)
	var matched []string
	for _, kw := range matchKeywords {
		if strings.Contains(lowerTitle, strings.ToLower(kw)) {
			matched = append(matched, kw)
		}
	}
	if len(matched) == 0 {
		return ""
	}
	return fmt.Sprintf("키워드 '%s' 포함", strings.Join(matched, ", "))
}

// noticeListSortOrderBy — sort 쿼리파라미터 화이트리스트. 사용자 입력을
// 그대로 ORDER BY에 꽂지 않고(SQL 인젝션 방지) 허용된 값만 고정 SQL로
// 매핑한다. status는 진행중(open)인 공고를 먼저 보여주고, 그 안에서는
// 최신순으로 정렬한다.
// 각 정렬 끝에 고유 tiebreaker(n.id DESC)를 붙인다 — 이게 없으면 published_at 등
// 정렬키가 같은(특히 지원사업의 NULL/동일 배치) 동점 행들의 순서가 OFFSET/LIMIT마다
// 달라져(실행계획 변화) 무한스크롤 페이지가 겹치고 같은 공고가 2~4개씩 중복 노출됐다
// (2026-08-14 수정). n.id는 유일하므로 전순서가 결정적 → 페이지 겹침/누락 제거.
var noticeListSortOrderBy = map[string]string{
	"new":      "n.published_at DESC NULLS LAST, n.id DESC",
	"deadline": "n.application_end_at ASC NULLS LAST, n.id DESC",
	"budget":   "n.budget_amount DESC NULLS LAST, n.id DESC",
	"name":     "n.title ASC, n.id DESC",
	"status":   "(CASE WHEN n.status = 'open' THEN 0 ELSE 1 END) ASC, n.published_at DESC NULLS LAST, n.id DESC",
}

func (s *Server) handleListNotices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	region := q.Get("region")
	industry := q.Get("industry")
	keyword := q.Get("q")
	userID, loggedIn := s.currentUserID(r)

	orderBy, ok := noticeListSortOrderBy[q.Get("sort")]
	if !ok {
		orderBy = noticeListSortOrderBy["new"]
	}

	offset := parseListingIntParam(q.Get("offset"), 0)
	limit := parseListingIntParam(q.Get("limit"), defaultNoticeListLimit)
	if limit <= 0 || limit > maxNoticeListLimit {
		limit = defaultNoticeListLimit
	}

	args := []any{}
	argN := 0
	addArg := func(v any) string {
		argN++
		args = append(args, v)
		return "$" + itoa(argN)
	}

	// LEFT JOIN + NULL 파라미터 트릭: 비로그인이면 sql.NullString{Valid:false}를
	// 바인딩해서 "x = NULL"이 항상 거짓이 되게 만든다 — listRequiredDocuments가
	// 이미 쓰고 있는 것과 같은 패턴(별도 인증 분기 없이 isBookmarked가 자연히 false).
	// COUNT(*) OVER()로 전체 매칭 건수를 같은 쿼리 안에서 함께 받는다 — 운영
	// 규모(2천건 미만)에서는 페이지당 별도 COUNT 쿼리를 또 날릴 필요가 없다.
	query := `
		SELECT n.id, n.notice_type, n.title, n.organization_name, n.region, n.industry, n.status,
		       n.application_end_at, n.budget_amount, n.official_url, n.current_version,
		       (nb.id IS NOT NULL) AS is_bookmarked, n.region_restricted, n.industry_restricted,
		       EXISTS(SELECT 1 FROM notice_changes nc WHERE nc.notice_id = n.id AND nc.created_at >= now() - interval '7 days') AS recently_changed,
		       (SELECT nv.enrichment_status FROM notice_versions nv WHERE nv.notice_id = n.id AND nv.version_number = n.current_version) AS enrichment_status,
		       COALESCE((SELECT array_agg(pr.region_name ORDER BY pr.sort_no) FROM notice_participation_regions pr JOIN notice_versions nv2 ON nv2.id = pr.notice_version_id WHERE nv2.notice_id = n.id AND nv2.version_number = n.current_version), '{}') AS official_regions,
		       COUNT(*) OVER() AS total_count
		FROM notices n
		LEFT JOIN notice_bookmarks nb ON nb.notice_id = n.id AND nb.user_id = ` + addArg(sql.NullString{String: userID, Valid: loggedIn}) + `
		WHERE 1=1`
	// includeClosed=1이 없으면 진행중 공고만 — 2026-08-06, 검색결과
	// 3,920건 중 25.8%(1,013건)가 이미 마감일이 지났는데도 status는
	// 'open'으로 남아 전부 섞여 나오던 걸 발견해 추가. status 컬럼은
	// 거의 항상 'open'이라(수집기가 마감을 감지해 status를 바꿔주는
	// 경로가 없음 — bizinfo.go/g2b.go 관례상 의도된 설계, 실제 마감
	// 판정은 항상 application_end_at로 한다) 주 판단 기준은 날짜다.
	// status 체크는 명시적으로 취소된(cancelled) 공고까지 같이 걸러내는
	// 보조 조건 — dashboard.go/growth_analytics.go 등 기존 재계산
	// 쿼리들이 이미 쓰고 있는 것과 동일한 조건을 그대로 재사용한다.
	if q.Get("includeClosed") != "1" {
		query += " AND n.status NOT IN ('closed','cancelled') AND (n.application_end_at IS NULL OR n.application_end_at >= CURRENT_DATE)"
	}
	if region != "" {
		query += " AND n.region = " + addArg(region)
	}
	if industry != "" {
		// 업종이 대분류 그룹명이면 raw값 집합으로 확장 매칭(맞춤공고 "결과 보기"가
		// 그룹명을 넘기는데 notices.industry엔 raw값만 있어 정확일치로는 0건이던
		// 문제 해결). 그룹이 아니면(사용자가 raw값 직접 입력) 기존 정확일치 유지.
		if raws, isGroup := industryGroupToRaws[industry]; isGroup {
			query += " AND n.industry = ANY(" + addArg(pq.Array(raws)) + ")"
		} else {
			query += " AND n.industry = " + addArg(industry)
		}
	}
	if keyword != "" {
		query += " AND n.title ILIKE " + addArg("%"+keyword+"%")
	}
	// noticeType/organizationName/budgetMin/budgetMax/keywordsInclude/
	// keywordsExclude — 2026-08-06 "맞춤공고"(saved_searches) 매칭용으로
	// 추가. 별도 매칭 쿼리를 새로 만들지 않고 이 목록 API 자체를 확장해서,
	// 저장된 조건의 "결과 보기"가 일반 공고검색과 동일한 화면/캐시/
	// 무한스크롤을 그대로 쓴다. q/region/industry와 마찬가지로 전부 선택
	// 파라미터라 기존 호출부(일반 검색 화면)는 영향받지 않는다.
	if noticeType := q.Get("noticeType"); noticeType != "" {
		query += " AND n.notice_type = " + addArg(noticeType)
	}
	if org := q.Get("organizationName"); org != "" {
		query += " AND n.organization_name ILIKE " + addArg("%"+org+"%")
	}
	if budgetMinRaw := q.Get("budgetMin"); budgetMinRaw != "" {
		if v, err := strconv.ParseInt(budgetMinRaw, 10, 64); err == nil {
			query += " AND n.budget_amount >= " + addArg(v)
		}
	}
	if budgetMaxRaw := q.Get("budgetMax"); budgetMaxRaw != "" {
		if v, err := strconv.ParseInt(budgetMaxRaw, 10, 64); err == nil {
			query += " AND n.budget_amount <= " + addArg(v)
		}
	}
	if includeKeywords := splitNonEmpty(q.Get("keywordsInclude")); len(includeKeywords) > 0 {
		var orParts []string
		for _, kw := range includeKeywords {
			orParts = append(orParts, "n.title ILIKE "+addArg("%"+kw+"%"))
		}
		query += " AND (" + strings.Join(orParts, " OR ") + ")"
	}
	if excludeRaw := q.Get("keywordsExclude"); excludeRaw != "" {
		for _, kw := range strings.Split(excludeRaw, ",") {
			kw = strings.TrimSpace(kw)
			if kw == "" {
				continue
			}
			query += " AND n.title NOT ILIKE " + addArg("%"+kw+"%")
		}
	}
	query += " ORDER BY " + orderBy + " LIMIT " + addArg(limit) + " OFFSET " + addArg(offset)

	// matchReasons 계산용 필터 목록 — 위 WHERE절에 쓴 것과 정확히 같은
	// 값이어야 한다(같은 q.Get 호출). region/industry/keyword는 변수명이
	// row 스캔 루프 안에서 스캔 결과로 가려지므로 여기서 별도 이름으로
	// 미리 떼어둔다. 지역/업종/발주기관/예산범위는 전부 WHERE절의 AND
	// 조건이라 이 쿼리로 나온 행은 이미 전부 만족한 상태 — 그래서 "이
	// 필터가 걸려 있었는지"만 보면 되고, row별 값을 다시 확인할 필요는
	// 없다. 포함 키워드만 OR 조건이라 어떤 키워드가 실제로 이 제목에
	// 걸렸는지 row마다 다시 확인해야 한다(computeKeywordMatchReason).
	baseMatchReasons := computeBaseMatchReasons(region, industry, q.Get("organizationName"), q.Get("budgetMin"), q.Get("budgetMax"))
	matchKeywords := splitNonEmpty(q.Get("keywordsInclude"))

	// scoringCompany — 로그인 + 회사 프로필이 있을 때만 채워서 아래 row
	// 루프에서 바로 등급을 계산한다(회사 정보를 못 구하면 grade는 그냥
	// 비워둔다 — company_pipeline.go attachPipelineGrades와 동일한
	// best-effort 원칙, 검색 자체를 막지 않는다).
	var scoringCompany *companyScoringInput
	var pipelineCompanyID string // 목록의 파이프라인 담김 상태 조회용(로그인+프로필일 때만)
	if loggedIn {
		if profile, err := s.getCompanyProfile(r, userID); err == nil && profile != nil {
			pipelineCompanyID = profile.ID
			var profRegion, profSize sql.NullString
			if profile.Region != nil {
				profRegion = sql.NullString{String: *profile.Region, Valid: true}
			}
			if profile.CompanySize != nil {
				profSize = sql.NullString{String: *profile.CompanySize, Valid: true}
			}
			trackRecordMax, err := s.fetchTrackRecordMaxAmount(r.Context(), profile.ID)
			if err != nil {
				s.logger.Error("list notices: track record lookup failed", "error", err)
			}
			scoringCompany = &companyScoringInput{Region: profRegion, Industry: profile.Industry, Size: profSize, TrackRecordMaxAmount: trackRecordMax}
		}
	}

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		s.logger.Error("list notices query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []noticeListItem{}
	total := 0
	for rows.Next() {
		var it noticeListItem
		var org, region, industry, officialURL, enrichStatus sql.NullString
		var budget sql.NullInt64
		var deadline sql.NullTime
		var regionRestricted, industryRestricted sql.NullBool
		var officialRegions pq.StringArray
		var totalCount int
		if err := rows.Scan(&it.ID, &it.NoticeType, &it.Title, &org, &region, &industry, &it.Status,
			&deadline, &budget, &officialURL, &it.CurrentVersion, &it.IsBookmarked, &regionRestricted, &industryRestricted,
			&it.RecentlyChanged, &enrichStatus, &officialRegions, &totalCount); err != nil {
			s.logger.Error("scan notice row failed", "error", err)
			continue
		}
		total = totalCount
		if regionRestricted.Valid {
			it.RegionRestricted = &regionRestricted.Bool
		}
		it.OrganizationName = org.String
		it.Region = region.String
		it.Industry = industry.String
		it.OfficialURL = officialURL.String
		if budget.Valid {
			it.BudgetAmount = &budget.Int64
		}
		if deadline.Valid {
			it.ApplicationEndAt = &deadline.Time
		}
		if scoringCompany != nil {
			score := scoreNoticeForCompany(noticeScoringInput{NoticeType: it.NoticeType, Region: region, Industry: industry, BudgetAmount: budget, IndustryRestricted: nullBoolPtr(industryRestricted),
				OfficialRegions: []string(officialRegions), RegionEnriched: regionEnrichedFromStatus(enrichStatus)}, *scoringCompany)
			it.Grade = score.Grade
			it.GradeReason = score.GradeReason
			it.JointVentureRecommended = score.JointVentureRecommended
			it.JointVentureReason = score.JointVentureReason
			// gradeFromCategories는 "참여 곤란" 등급에도 대부분 top-level
			// 사유를 안 채운다(카테고리별 Reason에만 있음) — 목록에서
			// "왜 참여 곤란인지"가 비어 보이면 등급 노출의 의미가 없으므로,
			// 이 등급일 때만 막힌 첫 카테고리 사유를 대신 채운다(참여
			// 권장/조건부는 배지만으로 충분해 그대로 비워둔다).
			if it.GradeReason == "" && score.Grade == gradeNotRecommended {
				for _, c := range score.Categories {
					if c.Result == "not_met" {
						it.GradeReason = c.Reason
						break
					}
				}
			}
		}
		if len(baseMatchReasons) > 0 || len(matchKeywords) > 0 {
			reasons := append([]string{}, baseMatchReasons...)
			if kwReason := computeKeywordMatchReason(matchKeywords, it.Title); kwReason != "" {
				reasons = append(reasons, kwReason)
			}
			it.MatchReasons = reasons
		}
		items = append(items, it)
	}

	// 파이프라인 담김 상태 매핑 — 이 페이지의 공고들 중 로그인 사용자 회사의
	// 파이프라인에 이미 있는 것에 entryId/status를 채운다(한 번의 배치 조회).
	// 목록 "참여검토" 버튼이 새로고침 후에도 담김 상태를 반영하고 토글/취소하게 한다.
	if pipelineCompanyID != "" && len(items) > 0 {
		ids := make([]string, len(items))
		for i, it := range items {
			ids[i] = it.ID
		}
		// notice_id는 uuid, $2는 text[](pq.Array) — uuid = ANY(text[])는 연산자가
		// 없어 500이 나므로 notice_id::text로 캐스팅해 비교한다.
		peRows, err := s.db.QueryContext(r.Context(),
			`SELECT notice_id, id, status FROM notice_pipeline_entries WHERE company_profile_id = $1 AND notice_id::text = ANY($2)`,
			pipelineCompanyID, pq.Array(ids))
		if err != nil {
			s.logger.Error("list notices: pipeline status lookup failed", "error", err)
		} else {
			type peInfo struct{ id, status string }
			byNotice := map[string]peInfo{}
			for peRows.Next() {
				var noticeID, id, status string
				if err := peRows.Scan(&noticeID, &id, &status); err == nil {
					byNotice[noticeID] = peInfo{id: id, status: status}
				}
			}
			peRows.Close()
			for i := range items {
				if pe, ok := byNotice[items[i].ID]; ok {
					items[i].PipelineEntryId = pe.id
					items[i].PipelineStatus = pe.status
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "count": len(items),
		"offset": offset, "limit": limit, "total": total,
		"hasMore": offset+len(items) < total,
	})
}

func (s *Server) handleGetNotice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	userID, loggedIn := s.currentUserID(r)

	var it noticeListItem
	var org, region, industry, officialURL, department, sourceName sql.NullString
	var budget sql.NullInt64
	var deadline, publishedAt, firstCollectedAt, lastVerifiedAt sql.NullTime
	var industryRestricted sql.NullBool

	err := s.db.QueryRowContext(r.Context(), `
		SELECT n.id, n.notice_type, n.title, n.organization_name, n.region, n.industry, n.status,
		       n.application_end_at, n.budget_amount, n.official_url, n.current_version,
		       (nb.id IS NOT NULL) AS is_bookmarked, n.department_name,
		       n.published_at, n.first_collected_at, n.last_verified_at, ds.name, n.industry_restricted
		FROM notices n
		LEFT JOIN notice_bookmarks nb ON nb.notice_id = n.id AND nb.user_id = $2
		LEFT JOIN data_sources ds ON ds.id = n.source_id
		WHERE n.id = $1`, id, sql.NullString{String: userID, Valid: loggedIn},
	).Scan(&it.ID, &it.NoticeType, &it.Title, &org, &region, &industry, &it.Status,
		&deadline, &budget, &officialURL, &it.CurrentVersion, &it.IsBookmarked, &department,
		&publishedAt, &firstCollectedAt, &lastVerifiedAt, &sourceName, &industryRestricted)

	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("get notice query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	it.OrganizationName, it.Region, it.Industry, it.OfficialURL = org.String, region.String, industry.String, officialURL.String
	if budget.Valid {
		it.BudgetAmount = &budget.Int64
	}
	if deadline.Valid {
		it.ApplicationEndAt = &deadline.Time
	}
	if publishedAt.Valid {
		it.PublishedAt = &publishedAt.Time
	}
	if firstCollectedAt.Valid {
		it.FirstCollectedAt = &firstCollectedAt.Time
	}
	if lastVerifiedAt.Valid {
		it.LastVerifiedAt = &lastVerifiedAt.Time
	}
	it.SourceName = sourceName.String

	// on-view 보강 트리거(비동기, 응답 지연 없음) — 이 공고가 미보강 procurement면 참가가능지역/
	// 허용면허(입찰자격)를 즉시 채워 다음 로드에서 보이게 한다. 지원사업/이미보강/키미설정이면 no-op.
	s.TriggerNoticeEnrichmentOnView(id)

	changes, err := s.listChanges(r.Context(), id)
	if err != nil {
		s.logger.Error("list changes failed", "error", err)
	}

	// 로그인 + 회사 프로필이 있으면 이 자리에서 바로 참여 가능성을 계산해
	// 응답에 얹는다(비영속 — DB에 안 씀). "AI 참여 분석" 섹션이 상세 페이지
	// 로드 시 자동으로 채워지도록 하기 위함이며, 같은 계산을 dashboard.go의
	// scoreNoticeForCompany와 공유한다.
	var profileID string
	var score *participationScore
	var company companyScoringInput
	confidenceTier := "basic"
	if loggedIn {
		companyProfile, err := s.getCompanyProfile(r, userID)
		if err != nil {
			s.logger.Error("get notice: profile lookup failed", "error", err)
		}
		if err == nil && companyProfile != nil {
			profileID = companyProfile.ID
			var companyRegion, companySize sql.NullString
			if companyProfile.Region != nil {
				companyRegion = sql.NullString{String: *companyProfile.Region, Valid: true}
			}
			if companyProfile.CompanySize != nil {
				companySize = sql.NullString{String: *companyProfile.CompanySize, Valid: true}
			}
			companyIndustry := pq.StringArray(companyProfile.Industry)
			trackRecordMax, err := s.fetchTrackRecordMaxAmount(r.Context(), profileID)
			if err != nil {
				s.logger.Error("get notice: track record max amount query failed", "error", err)
			}
			company = companyScoringInput{
				Region: companyRegion, Industry: []string(companyIndustry), Size: companySize,
				TrackRecordMaxAmount: trackRecordMax,
			}
			regionAuths, raErr := s.regionAuthoritiesByNoticeIDs(r.Context(), []string{id})
			if raErr != nil {
				s.logger.Error("get notice: region authority lookup failed", "error", raErr)
			}
			computed := scoreNoticeForCompany(
				noticeScoringInput{NoticeType: it.NoticeType, Region: region, Industry: industry, BudgetAmount: budget, IndustryRestricted: nullBoolPtr(industryRestricted),
					OfficialRegions: regionAuths[id].OfficialRegions, RegionEnriched: regionAuths[id].Enriched},
				company,
			)
			score = &computed

			hasPreciseData, err := s.profileHasPreciseJudgementData(r.Context(), profileID)
			if err != nil {
				s.logger.Error("get notice: confidence tier check failed", "error", err)
			}
			if hasPreciseData {
				confidenceTier = "precise"
			}
		}
	}

	eligibilityConditions := []eligibilityConditionItem{}
	requiredDocuments := []requiredDocumentItem{}
	licenseMatches := []licenseRequirementMatch{}
	attachments := []attachmentItem{}
	documentAnalysisStatus := ""
	var rawDetail *noticeRawDetail
	var aiSummary *noticeAISummary
	participationRegions := []string{}
	licenseLimits := []licenseLimitItem{}
	// participationRegionStatus — 지역 데이터 상태(2026-08-11 authoritative source):
	//   restricted   : 공식 참가가능지역 제한이 있음(그 목록 표시)
	//   confirmed_all: enrichment 완료 + 제한 없음 → 전국 확정(공식 확인)
	//   unknown      : 아직 공식 지역 미수집 → 추론값(notices.region)을 확정처럼 표시하지 않는다
	participationRegionStatus := "unknown"
	versionID, err := s.currentVersionID(r.Context(), id, it.CurrentVersion)
	if err != nil {
		s.logger.Error("get notice: current version lookup failed", "error", err)
	} else {
		eligibilityConditions, err = s.listEligibilityConditions(r.Context(), versionID)
		if err != nil {
			s.logger.Error("list eligibility conditions failed", "error", err)
		}
		requiredDocuments, err = s.listRequiredDocuments(r.Context(), versionID, profileID)
		if err != nil {
			s.logger.Error("list required documents failed", "error", err)
		}
		// documentAnalysisStatus — 2026-08-07, 빈 상태 문구 개선. 참가자격
		// 요건/제출서류 둘 다 비어있을 때만 계산한다(있으면 애초에 빈
		// 상태 문구 자체가 안 뜨니 불필요한 쿼리).
		if len(eligibilityConditions) == 0 && len(requiredDocuments) == 0 {
			documentAnalysisStatus, err = s.computeNoticeDocumentAnalysisStatus(r.Context(), versionID)
			if err != nil {
				s.logger.Error("compute document analysis status failed", "error", err)
			}
		}
		if profileID != "" {
			licenseMatches, err = s.matchNoticeLicenseRequirements(r.Context(), versionID, profileID)
			if err != nil {
				s.logger.Error("match notice license requirements failed", "error", err)
			}
		}
		attachments, err = s.listAttachments(r.Context(), versionID)
		if err != nil {
			s.logger.Error("list attachments failed", "error", err)
		}
		rawDetail, err = s.fetchNoticeRawDetail(r.Context(), versionID)
		if err != nil {
			s.logger.Error("fetch notice raw detail failed", "error", err)
		}
		aiSummary, err = s.fetchNoticeAISummary(r.Context(), versionID)
		if err != nil {
			s.logger.Error("fetch notice AI summary failed", "error", err)
		}
		// Phase C — 공식 오퍼레이션 보강분(참가가능지역/허용면허). 미보강이면 빈 목록.
		participationRegions, err = s.listParticipationRegions(r.Context(), versionID)
		if err != nil {
			s.logger.Error("list participation regions failed", "error", err)
		}
		licenseLimits, err = s.listLicenseLimits(r.Context(), versionID)
		if err != nil {
			s.logger.Error("list license limits failed", "error", err)
		}
		if len(participationRegions) > 0 {
			participationRegionStatus = "restricted"
		} else if auths, aerr := s.regionAuthoritiesByVersions(r.Context(), []string{versionID}); aerr != nil {
			s.logger.Error("get notice: region authority lookup failed", "error", aerr)
		} else if auths[versionID].Enriched {
			participationRegionStatus = "confirmed_all"
		}
	}

	var impact *changeImpact
	if score != nil && versionID != "" {
		impactAuths, _ := s.regionAuthoritiesByVersions(r.Context(), []string{versionID})
		impact, err = s.computeLatestChangeImpact(r.Context(), versionID,
			noticeScoringInput{NoticeType: it.NoticeType, Region: region, Industry: industry, BudgetAmount: budget, IndustryRestricted: nullBoolPtr(industryRestricted),
				OfficialRegions: impactAuths[versionID].OfficialRegions, RegionEnriched: impactAuths[versionID].Enriched}, company, *score)
		if err != nil {
			s.logger.Error("compute change impact failed", "error", err)
			impact = nil
		}
	}

	checkedCount := 0
	for _, d := range requiredDocuments {
		if d.Checked {
			checkedCount++
		}
	}

	// 경쟁사/낙찰이력: notice_award_history가 비어 있어도(수집기가 아직
	// 없음) awardHistory는 count=0인 정상 응답을 내려준다 — 프론트가
	// 이 경우 "아직 수집된 낙찰 이력이 없습니다"로 자연스럽게 표시.
	awardHistory, err := s.fetchOrganizationAwardHistory(r.Context(), it.OrganizationName, department.String)
	if err != nil {
		s.logger.Error("fetch organization award history failed", "error", err)
	}
	var hasCompetitiveOverlap bool
	if profileID != "" {
		hasCompetitiveOverlap, err = s.hasTrackRecordOverlap(r.Context(), profileID, it.OrganizationName, industry.String)
		if err != nil {
			s.logger.Error("check track record overlap failed", "error", err)
		}
	}

	// 원클릭 참여검토(Phase 1): 이 공고를 이미 파이프라인에 올렸는지
	// 미리 알려줘, 재방문 시 "참여 검토 시작" 버튼 대신 바로 "진행 중 →
	// 이동" 링크를 보여줄 수 있게 한다. UNIQUE(company_profile_id,
	// notice_id) 인덱스를 그대로 타는 단건 조회라 비용이 낮다.
	var existingPipelineEntryID *string
	if profileID != "" {
		var eid string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT id FROM notice_pipeline_entries WHERE company_profile_id = $1 AND notice_id = $2`,
			profileID, id,
		).Scan(&eid)
		if err == nil {
			existingPipelineEntryID = &eid
		} else if err != sql.ErrNoRows {
			s.logger.Error("get notice: existing pipeline entry lookup failed", "error", err)
		}
	}

	// 담당자 개인정보 마스킹 — system_admin은 원본, 그 외는 마스킹(이 공고에 파이프라인이
	// 있으면 [공개]용 원본도 함께). 권한/파이프라인 존재는 위에서 이미 계산한 값을 재사용.
	if rawDetail != nil {
		isAdmin := false
		if loggedIn {
			if role, rerr := s.userRole(r.Context(), userID); rerr == nil && role == "system_admin" {
				isAdmin = true
			}
		}
		applyOfficerMasking(rawDetail, isAdmin, existingPipelineEntryID != nil)
	}

	// supportDetail — B-2. 지원사업(support_program)일 때만 공식 데이터를 얹는다.
	// 입찰 공고는 support_program_details 행이 없어 nil(응답에서 null).
	var supportDetail *supportDetailDTO
	var supportConditions *supportConditionsDTO
	if it.NoticeType == "support_program" {
		supportDetail = s.fetchSupportProgramDetail(r.Context(), id)
		supportConditions = s.fetchSupportConditions(r.Context(), id) // B-3: 공고문 규칙 추출 상세조건
	}

	// 참여판정 신뢰성 확장(2026-08-09): 입찰 공고에 한해 기존 3요소 판정에
	// 면허·인증·직접생산확인을 이어붙여 조건별 PASS/REVIEW/FAIL/UNKNOWN을 만든다.
	// 지원사업은 판정 기준이 달라 제외. DB 변경 없음(조회 시점 계산).
	var partJudgment *participationJudgment
	if it.NoticeType == "procurement" && versionID != "" {
		partJudgment = s.buildParticipationJudgment(r.Context(), versionID, profileID, score, requiredDocuments)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"notice":                   it,
		"changes":                  changes,
		"eligibilityConditions":    eligibilityConditions,
		"requiredDocuments":        requiredDocuments,
		"licenseMatches":           licenseMatches,
		"documentAnalysisStatus":   documentAnalysisStatus,
		"documentReadiness":        map[string]int{"total": len(requiredDocuments), "checked": checkedCount},
		"attachments":              attachments,
		"detail":                   rawDetail,
		"supportDetail":            supportDetail,
		"supportConditions":        supportConditions,
		"participationScore":       score,
		"participationJudgment":    partJudgment,
		"confidenceTier":           confidenceTier,
		"aiSummary":                aiSummary,
		"changeImpact":             impact,
		"organizationAwardHistory": awardHistory,
		"hasCompetitiveOverlap":    hasCompetitiveOverlap,
		"existingPipelineEntryId":  existingPipelineEntryID,
		"participationRegions":     participationRegions,
		"participationRegionStatus": participationRegionStatus,
		"licenseLimits":            licenseLimits,
	})
}

func (s *Server) currentVersionID(ctx context.Context, noticeID string, currentVersion int) (string, error) {
	var versionID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM notice_versions WHERE notice_id = $1 AND version_number = $2`,
		noticeID, currentVersion,
	).Scan(&versionID)
	return versionID, err
}

type eligibilityConditionItem struct {
	Category         string  `json:"category"`
	ConditionName    string  `json:"conditionName"`
	SourceText       string  `json:"sourceText"`
	Confidence       float64 `json:"confidence"`
	ReviewStatus     string  `json:"reviewStatus"`
	ExtractionMethod string  `json:"extractionMethod"`
}

// listEligibilityConditions returns only document-derived rows (source_attachment_id
// IS NOT NULL) — it excludes the synthetic 지역/업종/예산규모 rows that
// handleEvaluateNotice auto-creates to satisfy eligibility_evaluations' FK,
// which carry condition_name "auto:region" etc. and aren't real extracted text.
// Rule-based and AI-supplemented rows are both included (extractionMethod
// tells the frontend which is which) — a rule-based review_required row
// isn't superseded just because an AI row exists alongside it, so hiding it
// would drop information rather than just deduplicate.
func (s *Server) listEligibilityConditions(ctx context.Context, versionID string) ([]eligibilityConditionItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT category, condition_name, source_text, confidence, review_status, extraction_method
		FROM eligibility_conditions
		WHERE notice_version_id = $1 AND source_attachment_id IS NOT NULL AND review_status != 'rejected'
		ORDER BY created_at`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []eligibilityConditionItem{}
	for rows.Next() {
		var it eligibilityConditionItem
		if err := rows.Scan(&it.Category, &it.ConditionName, &it.SourceText, &it.Confidence, &it.ReviewStatus, &it.ExtractionMethod); err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

type requiredDocumentItem struct {
	ID               string `json:"id"`
	DocumentName     string `json:"documentName"`
	SourceText       string `json:"sourceText"`
	IsRequired       bool   `json:"isRequired"`
	ExtractionMethod string `json:"extractionMethod"`
	Checked          bool   `json:"checked"`
}

// listRequiredDocuments takes profileID so it can report each item's
// checklist state for the current user. When profileID is "" (not logged
// in / no company profile), it's bound as SQL NULL — the join condition
// never matches NULL, so every item comes back unchecked without a
// separate query path.
func (s *Server) listRequiredDocuments(ctx context.Context, versionID, profileID string) ([]requiredDocumentItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rd.id, rd.document_name, COALESCE(rd.source_text, ''), rd.is_required, rd.extraction_method,
		       COALESCE(dci.is_checked, false)
		FROM required_documents rd
		LEFT JOIN document_checklist_items dci
		       ON dci.required_document_id = rd.id AND dci.company_profile_id = $2
		WHERE rd.notice_version_id = $1 AND rd.review_status != 'rejected'
		ORDER BY rd.document_name`, versionID, sql.NullString{String: profileID, Valid: profileID != ""})
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []requiredDocumentItem{}
	for rows.Next() {
		var it requiredDocumentItem
		if err := rows.Scan(&it.ID, &it.DocumentName, &it.SourceText, &it.IsRequired, &it.ExtractionMethod, &it.Checked); err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

type changeItem struct {
	Field      string    `json:"field"`
	OldValue   string    `json:"oldValue"`
	NewValue   string    `json:"newValue"`
	Importance string    `json:"importance"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (s *Server) listChanges(ctx context.Context, noticeID string) ([]changeItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT changed_field, COALESCE(old_value,''), COALESCE(new_value,''), importance, created_at
		FROM notice_changes WHERE notice_id = $1 ORDER BY created_at DESC LIMIT 50`, noticeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []changeItem
	for rows.Next() {
		var c changeItem
		if err := rows.Scan(&c.Field, &c.OldValue, &c.NewValue, &c.Importance, &c.CreatedAt); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
	})
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
