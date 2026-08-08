// pipeline_result_lookup.go — 우선순위5(2026-08-09). 개찰일시 기반 공식 결과
// 자동조회. 목적: 제출완료 이후 사용자가 낙찰/탈락 상태를 직접 관리하지 않게 한다.
//
// 대상: status='제출완료' AND opening_at IS NOT NULL AND opening_at <= now AND
// 아직 결과 미확정(result_finalized_at IS NULL). 개찰 후 결과가 바로 안 나올 수
// 있어 backoff(+30분/+2시간/+6시간/+24시간/+3일)로 조회하고, 소진하면 중단한다
// (무한조회 금지).
//
// 판정 원칙(공식 데이터로 확실한 경우만 상태 자동 변경):
//   - 자동 낙찰: 낙찰 raw의 사업자번호(bidwinnrBizno, 실측 100% 존재)가 우리 회사
//     사업자번호와 일치하고 최종확정(fnlSucsfDate)이면 제출완료→낙찰.
//   - 자동 탈락(보수적): 타사가 최종낙찰 + 재입찰 아님(rbidNo="000") + 안전한
//     경쟁 계약유형(success_bid_method_name)일 때만 제출완료→탈락. 협상/수의/
//     공동수급/규격가격동시/재입찰/유찰 등은 상태 유지(제출완료) + 내부 플래그.
//   - 사업자번호로 확정 못 하는데 업체명만 유사하면: 자동 전환하지 않고 "확인
//     하기" 후보 알림만.
//
// 사용자 노출 상태는 계속 6개만 유지한다 — 개찰대기/결과조회중/유찰/재입찰 같은
// 상태를 만들지 않고 result_type(내부)·배지·안내문구로 처리한다.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"biz-platform/collector/internal/collector/sources/scsbid"
)

// result_type(내부 플래그, 사용자 노출 X).
const (
	resultTypeWon         = "WON"
	resultTypeLost        = "LOST"
	resultTypeRebid       = "REBID"        // 재입찰(rbidNo!=000) — 상태 유지
	resultTypeNameMatch   = "NAME_MATCH"   // 업체명만 유사한 후보 — 자동전환 X, 알림만
	resultTypeNeedsReview = "NEEDS_REVIEW" // 보류 계약유형에서 타사낙찰 — 상태 유지
	resultTypeNoResult    = "NO_RESULT"    // backoff 소진했는데 결과 없음 — 조회 중단
)

// backoff 조회 스케줄(opening_at 기준 누적 오프셋). 길이만큼만 시도하고 중단한다.
var resultCheckOffsets = []time.Duration{
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
	72 * time.Hour,
}

// 판정 액션.
const (
	awardActionNone      = "NONE"       // 아직 결과 없음/미확정 → backoff 계속
	awardActionWin       = "WIN"        // 제출완료→낙찰
	awardActionLose      = "LOSE"       // 제출완료→탈락(보수적 조건 충족)
	awardActionHold      = "HOLD"       // 결과는 있으나 자동전환 보류(상태 유지)
	awardActionNameMatch = "NAME_MATCH" // 업체명 후보 알림만
)

type awardDecision struct {
	action     string
	resultType string
	reason     string // pipeline_status_history.reason(자동전환 시)
}

// classifyAwardResult — 순수 판정 함수(라이브 API 불필요, 단위테스트 대상).
// matched는 우리 공고번호에 해당하는 낙찰 레코드(없으면 nil).
func classifyAwardResult(matched *scsbid.AwardRecord, ourBizno, ourName, methodName string) awardDecision {
	if matched == nil {
		return awardDecision{awardActionNone, "", ""} // 아직 결과 없음
	}
	if strings.TrimSpace(matched.FnlSucsfDate) == "" {
		return awardDecision{awardActionNone, "", ""} // 개찰됐으나 최종확정 전
	}
	winnerBizno := normalizeBizno(matched.BidwinnrBizno)
	our := normalizeBizno(ourBizno)

	// 자동 낙찰 — 사업자번호 일치 + 최종확정.
	if winnerBizno != "" && our != "" && winnerBizno == our {
		return awardDecision{awardActionWin, resultTypeWon, "OFFICIAL_RESULT_MATCH"}
	}

	// 재입찰이면 상태 유지(재입찰 결과는 새 공고번호/차수에서 다뤄짐).
	if rb := strings.TrimSpace(matched.RbidNo); rb != "" && rb != "000" {
		return awardDecision{awardActionHold, resultTypeRebid, ""}
	}

	// 사업자번호로 확정 불가(방어적 — 실측상 거의 없음): 업체명 유사 후보 알림만.
	if winnerBizno == "" {
		if ourName != "" && companyNameMatch(matched.BidwinnrNm, ourName) {
			return awardDecision{awardActionNameMatch, resultTypeNameMatch, ""}
		}
		return awardDecision{awardActionHold, resultTypeNeedsReview, ""}
	}

	// 여기부터 타사가 최종낙찰(사업자번호가 우리와 다름).
	// 자동 탈락은 안전한 경쟁 계약유형에서만(보수적).
	if isSafeCompetitiveMethod(methodName) {
		return awardDecision{awardActionLose, resultTypeLost, "OFFICIAL_RESULT_OTHER_WINNER"}
	}
	// 협상/수의/공동수급/규격가격동시 등 — 자동전환 보류.
	return awardDecision{awardActionHold, resultTypeNeedsReview, ""}
}

// isSafeCompetitiveMethod — success_bid_method_name(낙찰방법명)이 "단일 최종
// 낙찰자가 명확한 일반경쟁"인지. 보류 키워드가 하나라도 있으면 false(보수적
// 기본값: 방법 미상/모호하면 자동 탈락 안 함).
func isSafeCompetitiveMethod(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	holdKeywords := []string{"협상", "수의", "공동", "규격가격동시", "우선협상", "다수공급", "단가", "2단계", "제안"}
	for _, k := range holdKeywords {
		if strings.Contains(n, k) {
			return false
		}
	}
	safeKeywords := []string{"적격심사", "최저가", "낙찰하한", "제한적최저가", "표준시장단가"}
	for _, k := range safeKeywords {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// normalizeBizno — 숫자만 남긴다("604-02-xxxxx" → "60402xxxxx").
func normalizeBizno(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// companyNameMatch — 업체명 정규화 후 완전일치(고신뢰 후보만). 자동 전환엔 절대
// 쓰지 않고 "확인하기" 후보 알림에만 쓴다.
func companyNameMatch(a, b string) bool {
	na, nb := normalizeCompanyName(a), normalizeCompanyName(b)
	return na != "" && na == nb
}

func normalizeCompanyName(s string) string {
	s = strings.TrimSpace(s)
	for _, tok := range []string{"주식회사", "(주)", "㈜", "(유)", "유한회사", "(재)", "(사)", " "} {
		s = strings.ReplaceAll(s, tok, "")
	}
	return s
}

// resultLookupStats — 실행 결과 요약(관리자 수동 트리거 응답/티커 로그용).
// Processed=이번에 실제 조회한 엔트리 수, Changed=낙찰/탈락 자동전환 수,
// Notifications=발송 알림 수, Errors=API/DB 오류 수.
type resultLookupStats struct {
	Processed     int
	Changed       int
	Notifications int
	Errors        int
}

// RunResultLookup은 티커/관리자 트리거 진입점.
func (s *Server) RunResultLookup(ctx context.Context) (resultLookupStats, error) {
	return s.runResultLookupAt(ctx, time.Now())
}

// handleRunResultLookup — 관리자 수동 트리거. 즉시 개찰 결과조회를 돌려 실행
// 결과 카운트를 반환한다. G2B_SERVICE_KEY 미설정이면 processed=0으로 정상 반환.
func (s *Server) handleRunResultLookup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	st, err := s.RunResultLookup(r.Context())
	if err != nil {
		s.logger.Error("run-result-lookup: batch failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "processed": st.Processed, "changed": st.Changed,
		"notifications": st.Notifications, "errors": st.Errors,
	})
}

// resultLookupRow — 조회 대상 한 건.
type resultLookupRow struct {
	entryID, noticeID, profileID string
	externalNoticeID             string
	title                        string
	org, industry, region        sql.NullString
	method                       sql.NullString
	openingAt                    time.Time
	attempts                     int
	ourBizno, ourName            string
}

func (s *Server) runResultLookupAt(ctx context.Context, now time.Time) (resultLookupStats, error) {
	var st resultLookupStats
	if s.scsbidSource == nil {
		return st, nil // G2B_SERVICE_KEY 미설정 — 결과조회 비활성
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT pe.id, pe.notice_id, pe.company_profile_id, n.external_notice_id, n.title,
		       n.organization_name, n.industry, n.region, n.success_bid_method_name, n.opening_at,
		       pe.result_check_attempts,
		       COALESCE(cp.business_registration_number,''), COALESCE(cp.company_name,'')
		FROM notice_pipeline_entries pe
		JOIN notices n ON n.id = pe.notice_id
		JOIN company_profiles cp ON cp.id = pe.company_profile_id
		WHERE pe.status = '제출완료'
		  AND n.opening_at IS NOT NULL
		  AND n.opening_at <= $1
		  AND pe.result_finalized_at IS NULL
		  AND pe.result_check_attempts < $2`, now, len(resultCheckOffsets))
	if err != nil {
		return st, err
	}
	var targets []resultLookupRow
	for rows.Next() {
		var r resultLookupRow
		if err := rows.Scan(&r.entryID, &r.noticeID, &r.profileID, &r.externalNoticeID, &r.title,
			&r.org, &r.industry, &r.region, &r.method, &r.openingAt, &r.attempts, &r.ourBizno, &r.ourName); err != nil {
			continue
		}
		targets = append(targets, r)
	}
	if cerr := rows.Err(); cerr != nil {
		rows.Close()
		return st, cerr
	}
	rows.Close()

	for _, t := range targets {
		// backoff: 이번 시도(attempts) 예정시각 = opening_at + offsets[attempts].
		nextAt := t.openingAt.Add(resultCheckOffsets[t.attempts])
		if now.Before(nextAt) {
			continue // 아직 이번 조회 시점 전
		}
		st.Processed++
		s.checkOneResult(ctx, t, now, &st)
	}
	return st, nil
}

// checkOneResult — 한 엔트리에 대해 낙찰 결과를 조회·판정·적용한다. st에 실행
// 통계를 누적한다(관리자 트리거 응답용).
func (s *Server) checkOneResult(ctx context.Context, t resultLookupRow, now time.Time, st *resultLookupStats) {
	// 개찰일시 부근 창에서 낙찰 목록을 받아 우리 공고번호를 매칭한다.
	begin := t.openingAt.Add(-1 * time.Hour)
	end := t.openingAt.Add(resultCheckOffsets[t.attempts] + time.Hour)
	if end.After(now) {
		end = now
	}
	records, err := s.scsbidSource.FetchAwards(ctx, begin, end)
	if err != nil {
		s.logger.Error("result lookup: FetchAwards failed", "error", err, "entry", t.entryID)
		st.Errors++
		// 조회 실패도 시도 1회로 계산해 무한재시도를 막는다(다음 backoff에서 재시도).
		s.bumpResultAttempt(ctx, t.entryID, now)
		return
	}
	matched := pickMatchingAward(records, t.externalNoticeID)
	decision := classifyAwardResult(matched, t.ourBizno, t.ourName, nz(t.method))

	// 시도 카운트/타임스탬프 갱신(모든 경로 공통).
	attemptsNow := t.attempts + 1
	exhausted := attemptsNow >= len(resultCheckOffsets)

	switch decision.action {
	case awardActionWin:
		s.applyAwardWin(ctx, t, matched, now)
		st.Changed++
		st.Notifications++
	case awardActionLose:
		s.applyAwardLose(ctx, t, matched, now)
		st.Changed++
		st.Notifications++
	case awardActionNameMatch:
		s.finalizeResultOnly(ctx, t.entryID, resultTypeNameMatch, matched, now)
		s.notifyResultNameMatch(ctx, t, matched)
		st.Notifications++
	case awardActionHold:
		// 결과는 있으나 자동전환 보류(재입찰/보류유형). 상태 유지하고 조회 중단.
		s.finalizeResultOnly(ctx, t.entryID, decision.resultType, matched, now)
		if decision.resultType == resultTypeRebid {
			s.notifyResultRebid(ctx, t)
		} else {
			s.notifyResultNeedsReview(ctx, t)
		}
		st.Notifications++
	default: // NONE — 아직 결과 없음/미확정
		if exhausted {
			// backoff 소진 — 유찰 등으로 결과가 끝내 안 나옴. 조회 중단.
			s.finalizeResultOnly(ctx, t.entryID, resultTypeNoResult, nil, now)
		} else {
			s.bumpResultAttempt(ctx, t.entryID, now)
		}
	}
}

// pickMatchingAward — 우리 공고번호(external_notice_id=bidNtceNo)와 일치하는 낙찰
// 레코드를 고른다. 재입찰 차수가 여럿이면 최종확정된 것을, 그 중에선 rbidNo가
// 가장 큰(가장 최근 차수) 것을 우선한다.
func pickMatchingAward(records []scsbid.AwardRecord, externalNoticeID string) *scsbid.AwardRecord {
	ext := strings.TrimSpace(externalNoticeID)
	if ext == "" {
		return nil
	}
	var best *scsbid.AwardRecord
	for i := range records {
		r := &records[i]
		if strings.TrimSpace(r.BidNtceNo) != ext {
			continue
		}
		if best == nil {
			best = r
			continue
		}
		// 최종확정된 것 우선, 동률이면 rbidNo 큰 것.
		bestFinal := strings.TrimSpace(best.FnlSucsfDate) != ""
		curFinal := strings.TrimSpace(r.FnlSucsfDate) != ""
		if curFinal && !bestFinal {
			best = r
		} else if curFinal == bestFinal && strings.TrimSpace(r.RbidNo) > strings.TrimSpace(best.RbidNo) {
			best = r
		}
	}
	return best
}

// bumpResultAttempt — 조회는 했으나 아직 결과 없음: 시도 카운트만 증가.
func (s *Server) bumpResultAttempt(ctx context.Context, entryID string, now time.Time) {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE notice_pipeline_entries
		SET result_check_started_at = COALESCE(result_check_started_at, $2),
		    last_result_checked_at = $2,
		    result_check_attempts = result_check_attempts + 1,
		    result_type = COALESCE(result_type, 'PENDING'),
		    updated_at = now()
		WHERE id = $1`, entryID, now); err != nil {
		s.logger.Error("result lookup: bump attempt failed", "error", err, "entry", entryID)
	}
}

// finalizeResultOnly — 상태(제출완료)는 그대로 두고 결과유형만 확정해 조회를
// 중단한다(재입찰/보류/업체명후보/결과없음). matched가 있으면 낙찰자 정보도 저장.
func (s *Server) finalizeResultOnly(ctx context.Context, entryID, resultType string, matched *scsbid.AwardRecord, now time.Time) {
	var winnerBizno, winnerName any
	var awardRate any
	if matched != nil {
		if b := normalizeBizno(matched.BidwinnrBizno); b != "" {
			winnerBizno = b
		}
		if n := strings.TrimSpace(matched.BidwinnrNm); n != "" {
			winnerName = n
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(matched.SucsfbidRate), 64); err == nil {
			awardRate = v
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE notice_pipeline_entries
		SET result_check_started_at = COALESCE(result_check_started_at, $2),
		    last_result_checked_at = $2,
		    result_check_attempts = result_check_attempts + 1,
		    result_finalized_at = $2,
		    result_type = $3,
		    winner_bizno = COALESCE($4, winner_bizno),
		    winner_name = COALESCE($5, winner_name),
		    award_rate = COALESCE($6, award_rate),
		    updated_at = now()
		WHERE id = $1`, entryID, now, resultType, winnerBizno, winnerName, awardRate); err != nil {
		s.logger.Error("result lookup: finalize-only failed", "error", err, "entry", entryID)
	}
}

// applyAwardWin — 제출완료→낙찰 자동전환 + 낙찰정보 저장 + 실적 후보 생성 + 알림.
func (s *Server) applyAwardWin(ctx context.Context, t resultLookupRow, m *scsbid.AwardRecord, now time.Time) {
	var awardAmount any
	if v, err := strconv.ParseInt(strings.TrimSpace(m.SucsfbidAmt), 10, 64); err == nil {
		awardAmount = v
	}
	var awardRate any
	if v, err := strconv.ParseFloat(strings.TrimSpace(m.SucsfbidRate), 64); err == nil {
		awardRate = v
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE notice_pipeline_entries
		SET status = '낙찰', decided_at = $2, result_finalized_at = $2, last_result_checked_at = $2,
		    result_check_started_at = COALESCE(result_check_started_at, $2),
		    result_check_attempts = result_check_attempts + 1,
		    result_type = $3,
		    awarded_amount = COALESCE($4, awarded_amount),
		    award_rate = COALESCE($5, award_rate),
		    winner_bizno = $6, winner_name = $7,
		    updated_at = now()
		WHERE id = $1 AND status = '제출완료'`,
		t.entryID, now, resultTypeWon, awardAmount, awardRate,
		nullIfEmpty(strPtr(normalizeBizno(m.BidwinnrBizno))), nullIfEmpty(strPtr(strings.TrimSpace(m.BidwinnrNm))))
	if err != nil {
		s.logger.Error("result lookup: apply win failed", "error", err, "entry", t.entryID)
		return
	}
	s.recordPipelineStatusChange(ctx, t.entryID, "제출완료", "낙찰", "SYSTEM", "OFFICIAL_RESULT_MATCH", "G2B_AWARD_RESULT")
	s.createTrackRecordCandidate(ctx, t, m)
	s.notifyResultWon(ctx, t, m)
}

// applyAwardLose — 제출완료→탈락 자동전환(보수적 조건 충족) + 경쟁 낙찰정보 저장 + 알림.
func (s *Server) applyAwardLose(ctx context.Context, t resultLookupRow, m *scsbid.AwardRecord, now time.Time) {
	var awardAmount any
	if v, err := strconv.ParseInt(strings.TrimSpace(m.SucsfbidAmt), 10, 64); err == nil {
		awardAmount = v
	}
	var awardRate any
	if v, err := strconv.ParseFloat(strings.TrimSpace(m.SucsfbidRate), 64); err == nil {
		awardRate = v
	}
	// awarded_amount는 "낙찰(우리) 금액" 의미라 탈락 건엔 넣지 않고, 경쟁사(타사)
	// 낙찰정보는 winner_* / award_rate에 저장한다(추후 탈락사유 분석용).
	_ = awardAmount
	_, err := s.db.ExecContext(ctx, `
		UPDATE notice_pipeline_entries
		SET status = '탈락', decided_at = $2, result_finalized_at = $2, last_result_checked_at = $2,
		    result_check_started_at = COALESCE(result_check_started_at, $2),
		    result_check_attempts = result_check_attempts + 1,
		    result_type = $3,
		    award_rate = COALESCE($4, award_rate),
		    winner_bizno = $5, winner_name = $6,
		    updated_at = now()
		WHERE id = $1 AND status = '제출완료'`,
		t.entryID, now, resultTypeLost, awardRate,
		nullIfEmpty(strPtr(normalizeBizno(m.BidwinnrBizno))), nullIfEmpty(strPtr(strings.TrimSpace(m.BidwinnrNm))))
	if err != nil {
		s.logger.Error("result lookup: apply lose failed", "error", err, "entry", t.entryID)
		return
	}
	s.recordPipelineStatusChange(ctx, t.entryID, "제출완료", "탈락", "SYSTEM", "OFFICIAL_RESULT_OTHER_WINNER", "G2B_AWARD_RESULT")
	s.notifyResultLost(ctx, t, m)
}

// createTrackRecordCandidate — 낙찰 시 실적 "후보"를 만든다(verified_at=NULL로
// 미확정 유지 — 이번 패치는 자동 실적 확정까지 하지 않는다). confidence='C'.
func (s *Server) createTrackRecordCandidate(ctx context.Context, t resultLookupRow, m *scsbid.AwardRecord) {
	var amount any
	if v, err := strconv.ParseInt(strings.TrimSpace(m.SucsfbidAmt), 10, 64); err == nil {
		amount = v
	}
	var contractDate any
	if d := strings.TrimSpace(m.FnlSucsfDate); d != "" {
		contractDate = d // 'YYYY-MM-DD'
	}
	project := strings.TrimSpace(t.title)
	if project == "" {
		project = "낙찰 공고 " + t.externalNoticeID
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO company_track_records
			(company_profile_id, project_name, client_name, contract_amount, contract_date,
			 industry_field, region, source_document_type, confidence, is_completed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, '기타', 'C', false)`,
		t.profileID, project, nz(t.org), amount, contractDate, nz(t.industry), nz(t.region)); err != nil {
		s.logger.Error("result lookup: track record candidate insert failed", "error", err, "entry", t.entryID)
	}
}

// --- 알림들(인앱+푸시; 이메일/SMS는 담당자 채널로) ---

func (s *Server) notifyResultWon(ctx context.Context, t resultLookupRow, m *scsbid.AwardRecord) {
	title := "🏆 낙찰 결과가 확인됐습니다 · " + t.title
	body := "공식 개찰 결과 귀사가 최종 낙찰자로 확인됐습니다."
	if amt := strings.TrimSpace(m.SucsfbidAmt); amt != "" {
		if v, err := strconv.ParseInt(amt, 10, 64); err == nil {
			body += fmt.Sprintf(" 낙찰금액 %s원.", formatWonAmount(v))
		}
	}
	s.dispatchResultNotification(ctx, t, "RESULT_WON", title, body)
}

func (s *Server) notifyResultLost(ctx context.Context, t resultLookupRow, m *scsbid.AwardRecord) {
	title := "개찰 결과가 확인됐습니다 · " + t.title
	body := "공식 개찰 결과 다른 업체가 최종 낙찰했습니다."
	if w := strings.TrimSpace(m.BidwinnrNm); w != "" {
		body += " 낙찰업체: " + w + "."
	}
	s.dispatchResultNotification(ctx, t, "RESULT_LOST", title, body)
}

func (s *Server) notifyResultNameMatch(ctx context.Context, t resultLookupRow, m *scsbid.AwardRecord) {
	title := "낙찰 결과 확인이 필요합니다 · " + t.title
	body := "귀사와 동일한 업체명으로 보이는 낙찰 결과가 확인됐습니다. 결과를 확인해주세요."
	s.dispatchResultNotification(ctx, t, "RESULT_NAME_MATCH", title, body)
}

func (s *Server) notifyResultRebid(ctx context.Context, t resultLookupRow) {
	title := "유찰/재입찰이 확인됐습니다 · " + t.title
	body := "이번 입찰은 유찰되어 재입찰이 예정됐습니다. 새 공고가 확인되면 안내하겠습니다."
	s.dispatchResultNotification(ctx, t, "RESULT_REBID", title, body)
}

func (s *Server) notifyResultNeedsReview(ctx context.Context, t resultLookupRow) {
	title := "개찰 결과를 확인하고 있습니다 · " + t.title
	body := "개찰 결과가 확인됐으나 계약 방식상 자동 판정이 어려워 확인이 필요합니다."
	s.dispatchResultNotification(ctx, t, "RESULT_NEEDS_REVIEW", title, body)
}

// dispatchResultNotification — 인앱/푸시(조직 전체) + 담당자 이메일/SMS.
func (s *Server) dispatchResultNotification(ctx context.Context, t resultLookupRow, eventType, title, body string) {
	entryID, noticeID := t.entryID, t.noticeID
	if err := s.insertEntryScopedInAppNotification(ctx, t.profileID, eventType, entryID, noticeID, title, body); err != nil {
		s.logger.Error("result lookup: in-app insert failed", "error", err)
	}
	s.sendPushToProfileMembers(ctx, t.profileID, title, body, "/#/pipeline/"+entryID)

	contacts, err := s.fetchNotifiableContacts(ctx, t.profileID, eventType, entryID)
	if err != nil {
		s.logger.Error("result lookup: contact lookup failed", "error", err)
		return
	}
	smsAllowed := s.smsAllowedForPlan(ctx, t.profileID)
	emailAllowed := s.checkEmailNotificationQuota(ctx, t.profileID)
	for _, c := range contacts {
		contactID := c.id
		if c.emailEnabled && c.email != "" {
			if emailAllowed {
				emailBody := "<p>" + html.EscapeString(body) + "</p><p><b>" + html.EscapeString(t.title) + "</b></p>"
				s.sendNotificationEmail(ctx, eventType, c.email, nil, &contactID, &entryID, &noticeID, title, emailBody)
			} else {
				s.logSkippedEmailNotification(ctx, eventType, c.email, nil, &contactID, &entryID, &noticeID, title)
			}
		}
		if smsAllowed && c.smsEnabled && c.phone != "" {
			s.sendNotificationSMS(ctx, eventType, c.phone, nil, &contactID, &entryID, &noticeID,
				fmt.Sprintf("%s %s", truncateForSMS(t.title, 20), body))
		}
	}
}

// nz — sql.NullString → 문자열(빈값 처리).
func nz(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func strPtr(s string) *string { return &s }
