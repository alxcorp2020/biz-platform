// dashboard_gaps.go — Phase 1 "기회 손실 분석" 정확화(2026-08-15).
//
// 기존 documentRequirementGaps(computeDocumentRequirementGaps)는 "그 서류를
// 요구하는 열린 공고 수"(요구빈도 proxy)라 "회사정보가 부족해 판정을 보류한
// 공고 수"와 의미가 다르다. 이 파일은 면허/인증/직접생산/실적을 실제 참여판정
// 규칙(buildParticipationJudgment 계열)과 동일 로직으로 대조해 "회사측 정보부족
// (company-side gap)"만 집계한다. 지역/업종/기업규모는 scoreNoticeForCompany의
// insufficient_data & DataGapSide=="company"를 재사용한다.
//
// 성능: 회사정보와 공고 요구조건을 요청당 각 1회씩 batch-load하고, 공고별로는
// 순수 in-memory 비교만 한다 — 공고 수에 비례하는 per-notice DB 쿼리(N+1)를
// 만들지 않는다. 총 쿼리 수는 열린 공고 수와 무관하게 상수(컨텍스트 3 + 스캔 1
// + 요구조건 2 = 6)다. buildParticipationJudgment 자체는 수정하지 않는다.
package api

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/lib/pq"
)

// participationGapSummary — 대시보드 "판정을 막고 있는 정보" 근거 데이터.
// Counts는 프론트 딥링크 키(FIELD_DEEP/GAP_DEEP)와 동일한 카테고리 키를 쓴다.
type participationGapSummary struct {
	// Counts[category] = 그 카테고리를 요구하는데 회사측 정보부족으로 판정을
	// 완료하지 못한 "열린 공고 수". 키: region/industry/companySize/license/
	// certification/directProduction/trackRecord.
	Counts map[string]int `json:"counts"`
	// NoticeTotal — 위 카테고리 중 하나라도 회사측 gap이 있는 "서로 다른" 공고 수
	// (중복 제거). "회사정보가 부족해 N개 공고를 최종 판정하지 못하고 있어요"의 근거.
	NoticeTotal int `json:"noticeTotal"`
}

type licenseCertState struct {
	status    string
	expiresAt sql.NullTime
}

// companyEligibilityContext — 요청당 1회 batch-load하는 회사측 판정 컨텍스트.
// 이후 공고별 비교는 전부 이 인메모리 값으로만 한다(추가 DB 조회 없음).
type companyEligibilityContext struct {
	licenseCertByName map[string]licenseCertState // TRIM(name) → 최신 상태(면허·인증 합집합)
	directProdStatus  string                      // 보유 | 미보유 | 확인되지않음
	hasTrackRecords   bool
}

// loadCompanyEligibilityContext — 회사 면허·인증·직접생산·실적을 각 1쿼리로 로드.
func (s *Server) loadCompanyEligibilityContext(ctx context.Context, profileID string) (companyEligibilityContext, error) {
	c := companyEligibilityContext{licenseCertByName: map[string]licenseCertState{}, directProdStatus: "확인되지않음"}

	// 면허 ∪ 인증(제출서류명과 정확일치 대조용). 같은 이름이 여러 개면 최신(created_at) 우선.
	rows, err := s.db.QueryContext(ctx, `
		SELECT btrim(name), status, expires_at FROM (
			SELECT name, status, expires_at, created_at FROM company_licenses WHERE company_profile_id = $1
			UNION ALL
			SELECT name, status, expires_at, created_at FROM company_certifications WHERE company_profile_id = $1
		) m ORDER BY created_at ASC`, profileID)
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var name, status string
		var exp sql.NullTime
		if rows.Scan(&name, &status, &exp) == nil {
			name = strings.TrimSpace(name)
			if name != "" {
				c.licenseCertByName[name] = licenseCertState{status: status, expiresAt: exp} // ASC라 최신이 마지막에 덮어씀
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return c, err
	}

	// 직접생산확인 tri-state(STEP 2-B와 동일 폴백: 새 컬럼 우선, NULL이면 legacy boolean).
	var st sql.NullString
	var legacy bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT direct_production_status, COALESCE(direct_production_cert,false) FROM company_profiles WHERE id = $1`, profileID,
	).Scan(&st, &legacy); err != nil {
		return c, err
	}
	switch {
	case st.Valid && strings.TrimSpace(st.String) != "":
		c.directProdStatus = st.String
	case legacy:
		c.directProdStatus = "보유"
	default:
		c.directProdStatus = "확인되지않음"
	}

	// 실적 보유 여부(1건 이상).
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM company_track_records WHERE company_profile_id = $1)`, profileID,
	).Scan(&c.hasTrackRecords); err != nil {
		return c, err
	}
	return c, nil
}

// gap 상태(내부용 — 새 공개 enum을 만들지 않는다). 집계는 "company_gap"만 센다.
const (
	gapMet       = "met"         // 조건 충족
	gapNotMet    = "not_met"     // 실제 미충족(회사가 답했으나 조건 불충족)
	gapCompany   = "company_gap" // 회사정보 부족(회사가 아직 등록/답변 안 함)
	gapNotReq    = "not_required"
	gapNoticeGap = "notice_gap" // 공고정보 부족(요구조건 미분석) — company_gap로 세지 않는다
)

// licenseCertGapState — matchLicenseCertByName과 동일 규칙의 순수 버전.
// 행 없음(미등록) = company_gap(=판정기 askable), 보유·유효 = met, 그 외 = not_met.
func (c companyEligibilityContext) licenseCertGapState(name string) string {
	st, ok := c.licenseCertByName[strings.TrimSpace(name)]
	if !ok {
		return gapCompany
	}
	if st.status == "보유" {
		if st.expiresAt.Valid && st.expiresAt.Time.Before(time.Now()) {
			return gapNotMet // 만료 → 미충족(판정기도 PASS 금지)
		}
		return gapMet
	}
	return gapNotMet // 미보유/확인되지않음/발급필요 → 답했으나 미충족
}

// classifyNoticeDocRequirements — 제출서류명 키워드로 요구 카테고리 감지.
// resolveNoticeRequirements(notice_requirements.go)의 required_documents 분기와
// 동일 규칙(직접생산 → 실적 → 면허 → 인증서/ISO 순).
func classifyNoticeDocRequirements(docNames []string) (licenseNames []string, certNames []string, reqDirect, reqTrack bool) {
	for _, raw := range docNames {
		n := strings.TrimSpace(raw)
		if n == "" {
			continue
		}
		switch {
		case strings.Contains(n, "직접생산"):
			reqDirect = true
		case strings.Contains(n, "실적"):
			reqTrack = true
		case strings.Contains(n, "면허"):
			licenseNames = append(licenseNames, n)
		case strings.Contains(n, "인증서") || strings.Contains(n, "ISO"):
			certNames = append(certNames, n)
		}
	}
	return
}

// anyCompanyGap — 이름 목록 중 하나라도 company_gap(미등록)이면 true(면허/인증 카테고리 판단).
func (c companyEligibilityContext) anyCompanyGap(names []string) bool {
	for _, n := range names {
		if c.licenseCertGapState(n) == gapCompany {
			return true
		}
	}
	return false
}

// computeParticipationGapCounts — 열린 공고 전체를 회사 컨텍스트와 대조해
// 카테고리별 "회사측 정보부족으로 판정 보류된 공고 수"를 센다(N+1 없음).
func (s *Server) computeParticipationGapCounts(ctx context.Context, profileID string, company companyScoringInput) (participationGapSummary, error) {
	out := participationGapSummary{Counts: map[string]int{}}
	if profileID == "" {
		return out, nil
	}

	cctx, err := s.loadCompanyEligibilityContext(ctx, profileID)
	if err != nil {
		return out, err
	}

	// 열린 공고 스캔(스코어링 입력 + notice_id). computeEligibilityBucketSummary와 동일 필터.
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.notice_type, n.region, n.industry, n.budget_amount, n.industry_restricted,
		       nv.enrichment_status,
		       COALESCE((SELECT array_agg(pr.region_name ORDER BY pr.sort_no) FROM notice_participation_regions pr WHERE pr.notice_version_id = nv.id), '{}')
		FROM notices n
		JOIN notice_versions nv ON nv.notice_id = n.id AND nv.version_number = n.current_version
		WHERE n.status NOT IN ('closed','cancelled')
		  AND (n.application_end_at IS NULL OR n.application_end_at >= CURRENT_DATE)
		LIMIT `+itoa(dashboardNoticeScanLimit))
	if err != nil {
		return out, err
	}
	type scanned struct {
		id                       string
		noticeType               string
		region, industry, enrich sql.NullString
		budget                   sql.NullInt64
		industryRestricted       sql.NullBool
		officialRegions          pq.StringArray
	}
	var notices []scanned
	ids := make([]string, 0, 128)
	for rows.Next() {
		var sc scanned
		if err := rows.Scan(&sc.id, &sc.noticeType, &sc.region, &sc.industry, &sc.budget, &sc.industryRestricted, &sc.enrich, &sc.officialRegions); err != nil {
			continue
		}
		notices = append(notices, sc)
		ids = append(ids, sc.id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(ids) == 0 {
		return out, nil
	}

	// 요구조건 batch-load(공고 수와 무관하게 각 1쿼리) — 현재버전, 반려 제외.
	docsByNotice := map[string][]string{}
	if drows, derr := s.db.QueryContext(ctx, `
		SELECT nv.notice_id, rd.document_name
		FROM required_documents rd
		JOIN notice_versions nv ON nv.id = rd.notice_version_id
		JOIN notices n ON n.id = nv.notice_id AND nv.version_number = n.current_version
		WHERE nv.notice_id = ANY($1) AND rd.review_status <> 'rejected'`, pq.Array(ids)); derr == nil {
		for drows.Next() {
			var nid, dname string
			if drows.Scan(&nid, &dname) == nil {
				docsByNotice[nid] = append(docsByNotice[nid], dname)
			}
		}
		drows.Close()
	} else {
		s.logger.Error("gap: required_documents batch failed", "error", derr)
	}
	trackCondNotice := map[string]bool{}
	if erows, eerr := s.db.QueryContext(ctx, `
		SELECT nv.notice_id
		FROM eligibility_conditions ec
		JOIN notice_versions nv ON nv.id = ec.notice_version_id
		JOIN notices n ON n.id = nv.notice_id AND nv.version_number = n.current_version
		WHERE nv.notice_id = ANY($1) AND ec.category = '실적' AND ec.review_status <> 'rejected'`, pq.Array(ids)); eerr == nil {
		for erows.Next() {
			var nid string
			if erows.Scan(&nid) == nil {
				trackCondNotice[nid] = true
			}
		}
		erows.Close()
	} else {
		s.logger.Error("gap: track eligibility batch failed", "error", eerr)
	}

	inc := func(key string) { out.Counts[key]++ }
	for _, sc := range notices {
		noticeHasGap := false

		// 지역/업종/기업규모 — scoreNoticeForCompany 재사용(회사측 insufficient_data만).
		score := scoreNoticeForCompany(
			noticeScoringInput{NoticeType: sc.noticeType, Region: sc.region, Industry: sc.industry, BudgetAmount: sc.budget,
				IndustryRestricted: nullBoolPtr(sc.industryRestricted), OfficialRegions: []string(sc.officialRegions), RegionEnriched: regionEnrichedFromStatus(sc.enrich)},
			company,
		)
		for _, cat := range score.Categories {
			if cat.Result == "insufficient_data" && cat.DataGapSide == "company" {
				if key := scoreCategoryToGapKey(cat.Category); key != "" {
					inc(key)
					noticeHasGap = true
				}
			}
		}

		// 면허/인증/직접생산/실적 — 제출서류·실적조건 기반 요구 감지 후 회사 컨텍스트와 대조.
		licNames, certNames, reqDirect, reqTrack := classifyNoticeDocRequirements(docsByNotice[sc.id])
		if trackCondNotice[sc.id] {
			reqTrack = true
		}
		if len(licNames) > 0 && cctx.anyCompanyGap(licNames) {
			inc("license")
			noticeHasGap = true
		}
		if len(certNames) > 0 && cctx.anyCompanyGap(certNames) {
			inc("certification")
			noticeHasGap = true
		}
		if reqDirect && cctx.directProdStatus == "확인되지않음" {
			inc("directProduction")
			noticeHasGap = true
		}
		if reqTrack && !cctx.hasTrackRecords {
			inc("trackRecord")
			noticeHasGap = true
		}

		if noticeHasGap {
			out.NoticeTotal++
		}
	}
	return out, nil
}

// scoreCategoryToGapKey — scoreNoticeForCompany 카테고리명 → 프론트 딥링크 키.
func scoreCategoryToGapKey(cat string) string {
	switch strings.TrimSpace(cat) {
	case "지역":
		return "region"
	case "업종":
		return "industry"
	case "예산 규모", "기업규모":
		return "companySize"
	}
	return ""
}
