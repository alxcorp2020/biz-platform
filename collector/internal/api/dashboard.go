// dashboard.go — GET /api/dashboard. 홈 화면 "오늘 할 일" 데이터: 진행 중
// 파이프라인(notice_pipeline_entries) 중 상태 미정/서류 미비인 것과, 아직
// 파이프라인에 없는 신규 추천 공고(grade='recommended', scoring.go의
// scoreNoticeForCompany 재사용 — DB에 아무것도 안 씀)를 합쳐 마감일순으로
// 보여준다. 이 응답을 쓰는 화면은 홈 하나뿐이라 예전 3버킷 집계
// (readyCount 등)는 완전히 대체한다(하위호환 유지 대상 아님).
package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

// dashboardNoticeScanLimit is a safety cap on how many active notices get
// scored per dashboard request (기존과 동일한 이유 — 1차 버전, 페이지네이션 없음).
const dashboardNoticeScanLimit = 500

// dashboardPriorityCloseSoonDays: 이 기간 내 마감이면 "마감임박"으로 집계.
const dashboardPriorityCloseSoonDays = 7

// pipelineActivePipelineStatuses: "종결"되지 않아 여전히 챙겨야 하는
// 파이프라인 상태 — 우선 업무 리스트/서류 카운트는 이 상태들만 대상으로 한다.
var pipelineActiveStatuses = map[string]bool{
	"검토전": true, "참여검토": true, "승인대기": true, "준비중": true,
}

// pipelineUndecidedStatuses: "상태가 아직 정해지지 않은" 단계 — 우선
// 업무 리스트 포함 조건의 절반(나머지 절반은 서류 미비 여부).
var pipelineUndecidedStatuses = map[string]bool{
	"검토전": true, "참여검토": true, "승인대기": true,
}

type dashboardPriorityItem struct {
	Kind             string     `json:"kind"` // "pipeline" | "recommendation" | "license_expiring" | "notice_change"
	NoticeID         string     `json:"noticeId"`
	PipelineEntryID  *string    `json:"pipelineEntryId,omitempty"`
	Title            string     `json:"title"`
	OrganizationName string     `json:"organizationName"`
	Status           *string    `json:"status,omitempty"` // pipeline 항목만
	ApplicationEndAt *time.Time `json:"applicationEndAt"`
	IsBookmarked     bool       `json:"isBookmarked"`
	// Reason/CtaLabel/CtaHref — Phase 2: "오늘 해야 할 일"이 항목마다 왜
	// 여기 있는지와 무엇을 누르면 되는지를 직접 말해준다("기관 · 상태"
	// 나열 대신). CtaHref를 서버가 확정해 내려주면 프론트는 종류별로
	// 다른 이동 규칙을 다시 구현할 필요가 없다(license_expiring/
	// notice_change처럼 pipelineEntryId도 없는 항목도 그대로 처리됨).
	Reason   string `json:"reason"`
	CtaLabel string `json:"ctaLabel"`
	CtaHref  string `json:"ctaHref"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("dashboard: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		// 프로필이 없는 것은 에러가 아니다 — 프론트가 온보딩 화면으로 분기한다.
		writeJSON(w, http.StatusOK, map[string]any{"hasProfile": false})
		return
	}
	profileID := profile.ID
	var region, size sql.NullString
	if profile.Region != nil {
		region = sql.NullString{String: *profile.Region, Valid: true}
	}
	if profile.CompanySize != nil {
		size = sql.NullString{String: *profile.CompanySize, Valid: true}
	}
	industryArr := pq.StringArray(profile.Industry)
	trackRecordMax, err := s.fetchTrackRecordMaxAmount(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: track record max amount query failed", "error", err)
	}
	company := companyScoringInput{
		Region: region, Industry: []string(industryArr), Size: size,
		TrackRecordMaxAmount: trackRecordMax,
	}

	bookmarkedIDs, err := s.fetchBookmarkedNoticeIDs(ctx, userID)
	if err != nil {
		s.logger.Error("dashboard: bookmarked notice ids query failed", "error", err)
	}

	pipelineRows, err := s.db.QueryContext(ctx, `
		SELECT pe.id, pe.notice_id, n.title, n.organization_name, pe.status,
		       pe.assignee_name, pe.submission_deadline
		FROM notice_pipeline_entries pe
		JOIN notices n ON n.id = pe.notice_id
		WHERE pe.company_profile_id = $1`, profileID)
	if err != nil {
		s.logger.Error("dashboard: pipeline entries query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	type pipelineRow struct {
		id, noticeID, title, status string
		organizationName            sql.NullString
		assigneeName                sql.NullString
		deadline                    sql.NullTime
	}
	var pipelineEntries []pipelineRow
	pipelinedNoticeIDs := map[string]bool{}
	for pipelineRows.Next() {
		var pr pipelineRow
		if err := pipelineRows.Scan(&pr.id, &pr.noticeID, &pr.title, &pr.organizationName,
			&pr.status, &pr.assigneeName, &pr.deadline); err != nil {
			continue
		}
		pipelineEntries = append(pipelineEntries, pr)
		pipelinedNoticeIDs[pr.noticeID] = true
	}
	pipelineRows.Close()
	if err := pipelineRows.Err(); err != nil {
		s.logger.Error("dashboard: pipeline entries scan failed", "error", err)
	}

	incompleteDocCounts, err := s.fetchIncompleteChecklistCounts(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: checklist counts query failed", "error", err)
	}

	reviewPendingCount, unassignedCount, needsDocumentCount, deadlineSoonCount := 0, 0, 0, 0
	priorityItems := []dashboardPriorityItem{}
	closeSoonCutoff := time.Now().AddDate(0, 0, dashboardPriorityCloseSoonDays)

	for _, pr := range pipelineEntries {
		if pipelineUndecidedStatuses[pr.status] {
			reviewPendingCount++
		}
		incomplete := incompleteDocCounts[pr.id]
		if pipelineActiveStatuses[pr.status] {
			needsDocumentCount += incomplete
			if pr.assigneeName.String == "" {
				unassignedCount++
			}
			// 마감임박 요약 카드는 파이프라인 항목만 센다(추천 공고 제외) —
			// 4개 요약 카드 전부 "#/pipeline?filter=..."로 들어갔을 때 보이는
			// 목록과 숫자가 정확히 일치해야 하는데, 아직 파이프라인에 넣지도
			// 않은 추천 공고는 그 목록에 나타날 수 없다. "오늘의 우선 업무"
			// 리스트(추천 포함)는 이 집계와 별개로 그대로 유지한다.
			if pr.deadline.Valid && pr.deadline.Time.Before(closeSoonCutoff) {
				deadlineSoonCount++
			}
		}
		if !pipelineActiveStatuses[pr.status] || (!pipelineUndecidedStatuses[pr.status] && incomplete == 0) {
			continue // 우선 업무 대상 아님: 종결됐거나, 상태도 정해지고 서류도 다 갖춰짐
		}
		entryID := pr.id
		status := pr.status
		item := dashboardPriorityItem{
			Kind: "pipeline", NoticeID: pr.noticeID, PipelineEntryID: &entryID,
			Title: pr.title, OrganizationName: pr.organizationName.String, Status: &status,
			IsBookmarked: bookmarkedIDs[pr.noticeID],
			CtaHref:      "#/pipeline/" + pr.id,
		}
		if pr.deadline.Valid {
			item.ApplicationEndAt = &pr.deadline.Time
		}
		// 사유는 우선순위 하나만 고른다(여러 이유가 겹칠 수 있어도 "지금
		// 당장 뭘 눌러야 하는지"는 하나로 좁혀야 행동이 명확해진다) —
		// 참여 여부 결정이 안 됐으면 그게 가장 근본적인 다음 행동이고,
		// 이미 결정됐다면 담당자 지정, 그것도 됐다면 서류 미비 순.
		switch {
		case pipelineUndecidedStatuses[pr.status]:
			item.Reason, item.CtaLabel = "참여 여부 결정 필요", "검토하기"
		case pr.assigneeName.String == "":
			item.Reason, item.CtaLabel = "담당자 지정 필요", "담당자 지정"
		default:
			item.Reason, item.CtaLabel = fmt.Sprintf("제출서류 %d건 확인 필요", incomplete), "서류 확인"
		}
		priorityItems = append(priorityItems, item)
	}

	noticeRows, err := s.db.QueryContext(ctx, `
		SELECT id, notice_type, title, organization_name, region, industry, budget_amount, application_end_at, industry_restricted
		FROM notices
		WHERE status NOT IN ('closed','cancelled')
		  AND (application_end_at IS NULL OR application_end_at >= CURRENT_DATE)
		ORDER BY application_end_at ASC NULLS LAST
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		s.logger.Error("dashboard: notices query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer noticeRows.Close()

	for noticeRows.Next() {
		var id, title, noticeType string
		var org, noticeRegion, noticeIndustry sql.NullString
		var budget sql.NullInt64
		var deadline sql.NullTime
		var industryRestricted sql.NullBool
		if err := noticeRows.Scan(&id, &noticeType, &title, &org, &noticeRegion, &noticeIndustry, &budget, &deadline, &industryRestricted); err != nil {
			continue
		}
		if pipelinedNoticeIDs[id] {
			continue // 이미 파이프라인에 있음 — 위에서 이미 처리됨
		}
		score := scoreNoticeForCompany(
			noticeScoringInput{NoticeType: noticeType, Region: noticeRegion, Industry: noticeIndustry, BudgetAmount: budget, IndustryRestricted: nullBoolPtr(industryRestricted)},
			company,
		)
		if score.Grade != gradeRecommended {
			continue
		}
		item := dashboardPriorityItem{
			Kind: "recommendation", NoticeID: id, Title: title, OrganizationName: org.String,
			IsBookmarked: bookmarkedIDs[id],
			Reason:       "참여 여부 결정 필요", CtaLabel: "검토하기", CtaHref: "#/notices/" + id,
		}
		if deadline.Valid {
			item.ApplicationEndAt = &deadline.Time
		}
		priorityItems = append(priorityItems, item)
	}
	if err := noticeRows.Err(); err != nil {
		s.logger.Error("dashboard: notices scan failed", "error", err)
	}

	// reviewNeededCount — 오늘의 AI 브리핑 한 문장의 근거. "오늘 해야 할
	// 일" 전체(인증만료/공고변경 포함)가 아니라 "참여 여부를 결정해야
	// 하는" 항목만 센다 — 브리핑 문장이 "검토할 사업이 N건"이라고 말하는데
	// 실제로는 인증 갱신 건이었다면 신뢰가 깎인다.
	reviewNeededCount := 0
	for _, it := range priorityItems {
		if it.Kind == "pipeline" || it.Kind == "recommendation" {
			reviewNeededCount++
		}
	}

	licenseExpiringItems, err := s.fetchLicenseExpiringItems(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: license expiring query failed", "error", err)
	}
	priorityItems = append(priorityItems, licenseExpiringItems...)

	noticeChangeItems, err := s.fetchNoticeChangeItems(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: notice change query failed", "error", err)
	}
	priorityItems = append(priorityItems, noticeChangeItems...)

	sort.Slice(priorityItems, func(i, j int) bool {
		a, b := priorityItems[i].ApplicationEndAt, priorityItems[j].ApplicationEndAt
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return a.Before(*b)
	})

	plan, err := s.effectivePlan(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: plan lookup failed", "error", err)
	}
	aiLimit, err := s.effectiveAIAnalysisLimit(ctx, profileID, plan)
	if err != nil {
		s.logger.Error("dashboard: AI limit lookup failed", "error", err)
	}
	aiUsed, err := s.countAIAnalysisThisMonth(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: AI usage count failed", "error", err)
	}

	automation, err := s.computeAutomationSummary(ctx, company)
	if err != nil {
		s.logger.Error("dashboard: automation summary failed", "error", err)
	}

	completeness, err := s.computeProfileCompleteness(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: profile completeness failed", "error", err)
	}

	// eligibilitySummary — Phase UX-01(2026-08-04) 대시보드 온보딩 UI/점진적
	// 정보요청용 4버킷 집계. 판정 로직(eligibility.go/scoring.go) 자체는
	// 안 건드리고 scoreNoticeForCompany의 결과만 다시 센다.
	eligibilitySummary, err := s.computeEligibilityBucketSummary(ctx, company)
	if err != nil {
		s.logger.Error("dashboard: eligibility bucket summary failed", "error", err)
	}

	// documentRequirementGaps — Phase UX-02(2026-08-04). eligibilitySummary와
	// 별개로, 실적/인증/직접생산확인처럼 판정 로직이 없는 카테고리의 "부족한
	// 정보 안내"를 실제 수치로 낸다(computeDocumentRequirementGaps 주석 참고).
	documentRequirementGaps, err := s.computeDocumentRequirementGaps(ctx, profileID)
	if err != nil {
		s.logger.Error("dashboard: document requirement gaps failed", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"hasProfile": true,
		"briefing":   dashboardBriefing(reviewNeededCount, len(priorityItems)),
		"summary": map[string]int{
			"reviewPendingCount":  reviewPendingCount,
			"deadlineSoonCount":   deadlineSoonCount,
			"needsDocumentCount":  needsDocumentCount,
			"unassignedCount":     unassignedCount,
			"aiAnalysisUsedCount": aiUsed,
			"aiAnalysisLimit":     aiLimit, // -1 = 무제한, 0 = 이 플랜에서 이용 불가(Free)
		},
		"priorityItems":     priorityItems,
		"automationSummary": automation,
		"growthSummary": map[string]int{
			"overallCompleteness": completeness.OverallCompleteness,
		},
		"eligibilitySummary":      eligibilitySummary,
		"documentRequirementGaps": documentRequirementGaps,
	})
}

// eligibilityBucketSummary — 열려있는 공고 전체를 회사 기준으로 다시
// 채점해 4버킷으로 센다. gradeDistributionForCompany(growth_analytics.go)와
// 같은 실시간 재채점 패턴 — 판정 로직 자체는 안 건드리고 그 결과만
// 다시 집계한다. "insufficientData"는 bucketFromCategories의 기존
// 3버킷(ready/needsReview/notRecommended)에는 없는 새 분류로, 회사측
// 정보 부족(DataGapSide=="company")으로 인한 insufficient_data가 하나라도
// 있고 not_met은 없을 때 여기로 뺀다 — 점진적 정보요청 UX가 "정확히 어떤
// 정보가 없어서" 판정이 안 됐는지 알아야 하기 때문(missingFields).
type eligibilityBucketSummary struct {
	Ready            int `json:"ready"`
	NeedsReview      int `json:"needsReview"`
	NotRecommended   int `json:"notRecommended"`
	InsufficientData int `json:"insufficientData"`
	// MissingFields — "region"/"industry"/"companySize" 중 회사 프로필에
	// 실제로 비어있는 값(사용자가 채우면 판정이 가능해짐). 결정적 순서를
	// 위해 정렬해서 내려준다(맵 순회는 비결정적).
	MissingFields []string `json:"missingFields"`
	// MissingFieldCounts — Phase UX-03(2026-08-04). MissingFields는 "어떤
	// 필드가 비어있는지"의 집합만 주지만, 단계별 정보요청 카드는 "이 필드를
	// 채우면 몇 건에 영향이 있는지" 실수치가 필요하다. 이미 계산 중인 판정
	// 결과를 필드별로 더 세밀하게 집계할 뿐 새 판정기준을 추가하는 게
	// 아니다 — eligibility.go/scoring.go는 전혀 안 건드림.
	MissingFieldCounts map[string]int `json:"missingFieldCounts"`
}

var eligibilityCategoryToProfileField = map[string]string{
	"지역": "region", "업종": "industry", "예산 규모": "companySize",
}

func (s *Server) computeEligibilityBucketSummary(ctx context.Context, company companyScoringInput) (eligibilityBucketSummary, error) {
	var summary eligibilityBucketSummary
	missingSet := map[string]bool{}
	missingCounts := map[string]int{}

	rows, err := s.db.QueryContext(ctx, `
		SELECT notice_type, region, industry, budget_amount, industry_restricted FROM notices
		WHERE status NOT IN ('closed','cancelled')
		  AND (application_end_at IS NULL OR application_end_at >= CURRENT_DATE)
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		return summary, err
	}
	defer rows.Close()

	for rows.Next() {
		var noticeType string
		var noticeRegion, noticeIndustry sql.NullString
		var budget sql.NullInt64
		var industryRestricted sql.NullBool
		if err := rows.Scan(&noticeType, &noticeRegion, &noticeIndustry, &budget, &industryRestricted); err != nil {
			continue
		}
		score := scoreNoticeForCompany(
			noticeScoringInput{NoticeType: noticeType, Region: noticeRegion, Industry: noticeIndustry, BudgetAmount: budget, IndustryRestricted: nullBoolPtr(industryRestricted)},
			company,
		)

		hasNotMet := false
		hasCompanyGap := false
		for _, c := range score.Categories {
			if c.Result == "not_met" {
				hasNotMet = true
			}
			if c.Result == "insufficient_data" && c.DataGapSide == "company" {
				hasCompanyGap = true
				if field, ok := eligibilityCategoryToProfileField[c.Category]; ok {
					missingSet[field] = true
					missingCounts[field]++
				}
			}
		}
		switch {
		case hasNotMet:
			summary.NotRecommended++
		case hasCompanyGap:
			summary.InsufficientData++
		case score.Bucket == "ready":
			summary.Ready++
		default:
			summary.NeedsReview++
		}
	}
	if err := rows.Err(); err != nil {
		return summary, err
	}

	for field := range missingSet {
		summary.MissingFields = append(summary.MissingFields, field)
	}
	sort.Strings(summary.MissingFields)
	summary.MissingFieldCounts = missingCounts
	return summary, nil
}

// documentRequirementGapItem — Phase UX-02(2026-08-04) "부족한 정보 안내"
// 실제 수치. eligibility.go/scoring.go의 판정 로직(met/not_met)은 실적/
// 인증/직접생산확인을 전혀 판단하지 못한다(지역/업종/예산규모만 판단) —
// 그렇다고 임의의 숫자를 보여줄 수는 없으므로, 이미 실제로 채워져 있는
// required_documents(공고별 실제 제출서류, document_extraction.go가
// 규칙+AI로 추출)를 키워드로 매칭해 "이 서류를 요구하는 열린 공고가 몇
// 건인가"라는 별도의 정직한 지표를 센다. 이건 "귀사가 이 공고에 참여
// 가능한가"라는 판정이 아니라 "이 공고들은 그 서류를 요구한다"는 사실
// 집계라 판정 로직을 전혀 건드리지 않고도 실제 숫자를 낼 수 있다. 회사가
// 해당 카테고리 데이터를 하나라도 이미 갖고 있으면(이미 등록했으면) 더 이상
// "부족한 정보"가 아니므로 응답에서 제외한다.
//
// ⚠️ 2026-08-04 Phase UX-05: NoticeCount==0인 항목도 이제 응답에 포함한다
// (원래는 count>0일 때만 넣었음). 실제 온보딩 흐름에서 확인해보니
// required_documents 자체가 아직 희박한 초기 상태에서는(공고가 첨부문서
// 처리를 아직 안 거쳤거나 요구서류 텍스트가 키워드와 안 맞으면) 이 세
// 카테고리가 전부 빠져 온보딩 카드 큐가 "지역/업종/기업규모" 딱 1개로만
// 끝나버리는 문제가 있었다(사용자 실제 재현 확인). NoticeCount==0은
// "정확한 영향 건수를 아직 모른다"는 뜻이지 "이 회사가 이 데이터를 이미
// 갖고 있다"는 뜻이 아니므로(hasAny==false는 위에서 이미 확인됨) 여전히
// "부족한 정보"가 맞다 — 프론트가 NoticeCount 유무로 문구만 다르게
// 보여준다("N건의 요구서류를 확인 못함" vs "아직 등록하지 않았습니다").
type documentRequirementGapItem struct {
	Category    string `json:"category"`
	Label       string `json:"label"`
	NoticeCount int    `json:"noticeCount"`
	CtaHref     string `json:"ctaHref"`
}

var documentRequirementCategories = []struct {
	Category   string
	Label      string
	Keywords   []string // required_documents.document_name에 대한 ILIKE 패턴(OR)
	CheckTable string   // 회사가 이미 데이터를 갖고 있는지 확인할 테이블(company_profile_id FK)
	CtaHref    string
}{
	{"trackRecord", "수행실적", []string{"%실적%"}, "company_track_records", "#/me/saved-searches"},
	// "면허증"(3글자)은 "인증서"와 겹치지 않는 별도 키워드 — company_licenses
	// (면허·신고·등록)는 company_certifications(인증서)와 다른 테이블이라
	// 온보딩 카드에서도 별개 후보로 다룬다(2026-08-04 재설계).
	{"license", "보유 면허", []string{"%면허증%"}, "company_licenses", "#/me/saved-searches"},
	// "인증"(2글자)은 "직접생산확인증명서"(확인+증명서) 안에 우연히 부분
	// 문자열로 들어있어 오탐된다(로컬 검증 중 실제 재현) — "인증서"(3글자)로
	// 좁혀서 이 충돌을 피한다.
	{"certification", "인증", []string{"%인증서%", "%ISO%"}, "company_certifications", "#/me/saved-searches"},
	// directProduction — CheckTable은 안 쓴다(아래 특수 분기 참고). 2026-08-04
	// Phase UX-03에서 발견: 원래 company_licenses에 아무 행이나 있으면(카테고리
	// 필터 없이) 충족된 걸로 잘못 체크하고 있었는데, 실제 #/me/saved-searches 화면의
	// "직접생산확인" 체크박스(company_profiles.direct_production_cert)는 이
	// 계산과 전혀 연결이 안 되어 있었다 — 사용자가 그 체크박스를 켜도 이 갭이
	// 안 사라지는 버그. computeDocumentRequirementGaps 안에서 이 카테고리만
	// direct_production_cert 컬럼값을 직접 확인하도록 고쳤다.
	{"directProduction", "직접생산확인", []string{"%직접생산%"}, "", "#/me/saved-searches"},
}

func (s *Server) computeDocumentRequirementGaps(ctx context.Context, profileID string) ([]documentRequirementGapItem, error) {
	var gaps []documentRequirementGapItem
	for _, cat := range documentRequirementCategories {
		var hasAny bool
		if cat.Category == "directProduction" {
			if err := s.db.QueryRowContext(ctx,
				`SELECT direct_production_cert FROM company_profiles WHERE id = $1`,
				profileID,
			).Scan(&hasAny); err != nil {
				return nil, err
			}
		} else if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM `+cat.CheckTable+` WHERE company_profile_id = $1)`,
			profileID,
		).Scan(&hasAny); err != nil {
			return nil, err
		}
		if hasAny {
			continue
		}

		conds := make([]string, len(cat.Keywords))
		args := make([]any, 0, len(cat.Keywords))
		for i, kw := range cat.Keywords {
			args = append(args, kw)
			conds[i] = fmt.Sprintf("rd.document_name ILIKE $%d", len(args))
		}
		query := `
			SELECT COUNT(DISTINCT n.id)
			FROM required_documents rd
			JOIN notice_versions nv ON nv.id = rd.notice_version_id
			JOIN notices n ON n.id = nv.notice_id AND nv.version_number = n.current_version
			WHERE rd.review_status != 'rejected'
			  AND n.status NOT IN ('closed','cancelled')
			  AND (n.application_end_at IS NULL OR n.application_end_at >= CURRENT_DATE)
			  AND (` + strings.Join(conds, " OR ") + `)`
		var count int
		if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			return nil, err
		}
		// count==0이어도 넣는다 — 위 doc comment 참고(Phase UX-05).
		gaps = append(gaps, documentRequirementGapItem{
			Category: cat.Category, Label: cat.Label, NoticeCount: count, CtaHref: cat.CtaHref,
		})
	}
	return gaps, nil
}

// dashboardBriefing — "오늘의 AI 브리핑" 한 문장. 참여 여부를 결정해야
// 하는 건(reviewNeededCount)을 우선 강조하고, 그게 0이어도 인증만료/공고
// 변경 같은 다른 할 일이 있으면 그 사실을 알려준다 — "확인 필요 500건"
// 처럼 원시 총량을 던지는 대신, 항상 "오늘 실제로 뭘 하면 되는지"로 문장을
// 맺는다.
func dashboardBriefing(reviewNeededCount, totalCount int) string {
	switch {
	case reviewNeededCount > 0:
		return fmt.Sprintf("오늘 우선 검토할 사업이 %d건 있어요.", reviewNeededCount)
	case totalCount > 0:
		return fmt.Sprintf("새로 검토할 사업은 없지만, 확인할 일이 %d건 있어요.", totalCount)
	default:
		return "오늘은 새로 확인할 업무가 없어요. 잘하고 계세요."
	}
}

// fetchIncompleteChecklistCounts returns, per pipeline entry id, how many
// checklist items are NOT status='보유' — used both for the
// needsDocumentCount summary tile and for deciding which pipeline entries
// belong in "오늘의 우선 업무" (서류 미비인 것).
func (s *Server) fetchIncompleteChecklistCounts(ctx context.Context, profileID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pe.id, count(*) FILTER (WHERE ci.status != '보유')
		FROM notice_pipeline_entries pe
		LEFT JOIN pipeline_checklist_items ci ON ci.pipeline_entry_id = pe.id
		WHERE pe.company_profile_id = $1
		GROUP BY pe.id`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			continue
		}
		counts[id] = n
	}
	return counts, rows.Err()
}

// fetchBookmarkedNoticeIDs는 이미 최대 500건을 스캔하는 메인 공고 쿼리에
// notice_bookmarks LEFT JOIN을 더 얹기보다, 사용자의 북마크 집합을 한 번만
// 조회해 맵으로 들고 있다가 추천 목록 생성 시 O(1)로 체크한다.
func (s *Server) fetchBookmarkedNoticeIDs(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT notice_id FROM notice_bookmarks WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// dashboardLicenseExpiringWindowDays — 이 기간 안에 만료되는 보유 면허/
// 인증만 "오늘 해야 할 일"에 올린다(스펙 6.1 예시: "만료까지 29일").
const dashboardLicenseExpiringWindowDays = 30

// fetchLicenseExpiringItems — company_licenses/certifications 중
// status='보유'이면서 만료가 임박한 것. 관련 공고가 없는 항목이라
// NoticeID/PipelineEntryID는 비워두고 CtaHref로 프로필 화면(면허·인증
// 탭)을 직접 가리킨다.
func (s *Server) fetchLicenseExpiringItems(ctx context.Context, profileID string) ([]dashboardPriorityItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, expires_at FROM (
			SELECT name, expires_at FROM company_licenses
			WHERE company_profile_id = $1 AND status = '보유' AND expires_at IS NOT NULL
			UNION ALL
			SELECT name, expires_at FROM company_certifications
			WHERE company_profile_id = $1 AND status = '보유' AND expires_at IS NOT NULL
		) x
		WHERE expires_at BETWEEN CURRENT_DATE AND CURRENT_DATE + INTERVAL '1 day' * $2
		ORDER BY expires_at ASC`,
		profileID, dashboardLicenseExpiringWindowDays,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []dashboardPriorityItem{}
	for rows.Next() {
		var name string
		var expiresAt time.Time
		if err := rows.Scan(&name, &expiresAt); err != nil {
			continue
		}
		daysLeft := int(time.Until(expiresAt).Hours() / 24)
		expiresAtCopy := expiresAt
		items = append(items, dashboardPriorityItem{
			Kind: "license_expiring", Title: name,
			Reason: fmt.Sprintf("만료까지 %d일", daysLeft), CtaLabel: "갱신 일정 등록",
			CtaHref: "#/me/saved-searches", ApplicationEndAt: &expiresAtCopy,
		})
	}
	return items, rows.Err()
}

// dashboardNoticeChangeWindowDays — 이 기간 안에 감지된 중요 변경만 올린다
// (계속 쌓이지 않도록 자연스럽게 만료되게).
const dashboardNoticeChangeWindowDays = 7

// fetchNoticeChangeItems — 진행 중인(active) 파이프라인 건 중 최근 중요
// 변경(critical/major)이 감지된 것. 공고당 가장 최근 변경 하나만 올린다
// (DISTINCT ON).
func (s *Server) fetchNoticeChangeItems(ctx context.Context, profileID string) ([]dashboardPriorityItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (n.id) n.id, n.title, n.organization_name, n.application_end_at,
		       nc.changed_field, nc.old_value, nc.new_value
		FROM notice_pipeline_entries pe
		JOIN notices n ON n.id = pe.notice_id
		JOIN notice_versions nv ON nv.notice_id = n.id AND nv.version_number = n.current_version
		JOIN notice_changes nc ON nc.to_version_id = nv.id
		WHERE pe.company_profile_id = $1
		  AND pe.status = ANY($2)
		  AND nc.importance IN ('critical','major')
		  AND nc.created_at >= now() - INTERVAL '1 day' * $3
		ORDER BY n.id, nc.created_at DESC`,
		profileID, pq.Array(activeStatusList()), dashboardNoticeChangeWindowDays,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []dashboardPriorityItem{}
	for rows.Next() {
		var id, title, changedField string
		var org, oldValue, newValue sql.NullString
		var deadline sql.NullTime
		if err := rows.Scan(&id, &title, &org, &deadline, &changedField, &oldValue, &newValue); err != nil {
			continue
		}
		item := dashboardPriorityItem{
			Kind: "notice_change", NoticeID: id, Title: title, OrganizationName: org.String,
			Reason:   describeNoticeChangeForDashboard(changedField, oldValue.String, newValue.String),
			CtaLabel: "변경사항 보기", CtaHref: "#/notices/" + id,
		}
		if deadline.Valid {
			item.ApplicationEndAt = &deadline.Time
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// activeStatusList — pipelineActiveStatuses(맵)를 pq.Array에 넘길 수 있는
// slice로. 맵 순서가 안 정해져도 IN 조건에는 영향 없다.
func activeStatusList() []string {
	out := make([]string, 0, len(pipelineActiveStatuses))
	for k := range pipelineActiveStatuses {
		out = append(out, k)
	}
	return out
}

// describeNoticeChangeForDashboard — application_end_at 변경(스펙 6.1 예시:
// "제출기한이 2일 연장되었습니다")은 늘어났는지 줄었는지까지 말해주고,
// 그 외 중요 필드는 일반 문구로 대체한다(과장 없이 "변경되었다"는 사실만).
func describeNoticeChangeForDashboard(field, oldValue, newValue string) string {
	if field == "application_end_at" {
		oldDate, oldErr := time.Parse("2006-01-02", oldValue)
		newDate, newErr := time.Parse("2006-01-02", newValue)
		if oldErr == nil && newErr == nil {
			diffDays := int(newDate.Sub(oldDate).Hours() / 24)
			switch {
			case diffDays > 0:
				return fmt.Sprintf("제출기한이 %d일 연장되었습니다.", diffDays)
			case diffDays < 0:
				return fmt.Sprintf("제출기한이 %d일 단축되었습니다.", -diffDays)
			}
		}
		return "제출기한이 변경되었습니다."
	}
	return "공고 내용이 변경되었습니다."
}

// dashboardAssumedMinutesPerNotice — "이번 달 자동화 성과"의 절감시간은
// 실측이 아니라 가정이다(과장 금지 원칙 — Phase 7 스펙과 동일). 응답에
// SavedMinutesAssumption으로 이 가정을 항상 함께 내려 프론트가 반드시
// 명시하게 한다.
const dashboardAssumedMinutesPerNotice = 5

type automationSummary struct {
	AnalyzedCount          int    `json:"analyzedCount"`
	NarrowedCount          int    `json:"narrowedCount"`
	ChangesTrackedCount    int    `json:"changesTrackedCount"`
	EstimatedSavedMinutes  int    `json:"estimatedSavedMinutes"`
	SavedMinutesAssumption string `json:"savedMinutesAssumption"`
}

// computeAutomationSummary — "이번 달"(달력 기준) 자동화 성과 4개 지표.
// analyzedCount/narrowedCount는 이번 달 새로 수집된 공고를 대상으로,
// changesTrackedCount는 그 안에서 실제로 중요 변경이 감지된 공고 수만
// 센다(전체 시스템의 변경이력이 아니라 "이 회사가 이번 달 마주친" 범위로
// 한정 — 그래야 "당신을 위해 자동화했다"는 문장이 성립한다).
func (s *Server) computeAutomationSummary(ctx context.Context, company companyScoringInput) (automationSummary, error) {
	summary := automationSummary{SavedMinutesAssumption: fmt.Sprintf("공고 1건당 검토 %d분 가정", dashboardAssumedMinutesPerNotice)}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, notice_type, region, industry, budget_amount, industry_restricted
		FROM notices
		WHERE created_at >= date_trunc('month', now())
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		return summary, err
	}
	var noticeIDs []string
	for rows.Next() {
		var id, noticeType string
		var region, industry sql.NullString
		var budget sql.NullInt64
		var industryRestricted sql.NullBool
		if err := rows.Scan(&id, &noticeType, &region, &industry, &budget, &industryRestricted); err != nil {
			continue
		}
		noticeIDs = append(noticeIDs, id)
		score := scoreNoticeForCompany(
			noticeScoringInput{NoticeType: noticeType, Region: region, Industry: industry, BudgetAmount: budget, IndustryRestricted: nullBoolPtr(industryRestricted)}, company,
		)
		if score.Grade == gradeRecommended {
			summary.NarrowedCount++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return summary, err
	}

	summary.AnalyzedCount = len(noticeIDs)
	summary.EstimatedSavedMinutes = summary.AnalyzedCount * dashboardAssumedMinutesPerNotice

	if len(noticeIDs) > 0 {
		if err := s.db.QueryRowContext(ctx, `
			SELECT count(DISTINCT notice_id) FROM notice_changes
			WHERE notice_id = ANY($1) AND importance IN ('critical','major')
			  AND created_at >= date_trunc('month', now())`,
			pq.Array(noticeIDs),
		).Scan(&summary.ChangesTrackedCount); err != nil {
			return summary, err
		}
	}
	return summary, nil
}
