// award_history_ingest.go — notice_award_history를 조달청 나라장터
// 낙찰정보서비스(ScsbidInfoService) 실데이터로 채우는 배치.
// RunPipelineAutoTransitions/RunDailyNotifications과 같은 패턴: notices
// 수집 파이프라인(collector.Collector)과 무관한 별도 함수로, cmd/apiserver의
// 일일 티커에서 호출한다. 낙찰 레코드는 "공고"가 아니라서(버전관리/변경감지
// 대상이 아님) collector.Collector로 만들 이유가 없다.
package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"biz-platform/collector/internal/collector/sources/scsbid"
)

// awardHistoryLookbackWindow — 매일 배치가 도는데도 어제자만 조회하지 않고
// 최근 7일을 매번 다시 조회한다: 실개찰(rlOpengDt) 이후 낙찰업체 확정/정정이
// 며칠 늦게 반영되는 경우가 있어, 하루만 보면 그 사이 갱신분을 영영
// 놓친다. ON CONFLICT (source_id, external_bid_id) DO UPDATE라 매번
// 재조회해도 중복 삽입되지 않고 최신값으로 갱신만 된다.
const awardHistoryLookbackWindow = 7 * 24 * time.Hour

// RunAwardHistoryIngestion fetches the last awardHistoryLookbackWindow of
// 용역 부문 낙찰 records from ScsbidInfoService and upserts them into
// notice_award_history. Returns the number of rows written (inserted or
// updated).
func (s *Server) RunAwardHistoryIngestion(ctx context.Context, src *scsbid.Source) (int, error) {
	sourceID, err := s.ensureScsbidDataSource(ctx)
	if err != nil {
		return 0, err
	}

	end := time.Now()
	begin := end.Add(-awardHistoryLookbackWindow)
	records, err := src.FetchAwards(ctx, begin, end)
	if err != nil {
		return 0, err
	}

	written := 0
	for _, rec := range records {
		orgName := strings.TrimSpace(rec.DminsttNm)
		if orgName == "" {
			continue // notice_award_history.organization_name is NOT NULL — skip unusable rows
		}
		externalBidID := strings.TrimSpace(rec.BidNtceNo) + "-" + strings.TrimSpace(rec.BidNtceOrd)

		var awardAmount *int64
		if v, err := strconv.ParseInt(rec.SucsfbidAmt, 10, 64); err == nil {
			awardAmount = &v
		}
		var awardRate *float64
		if v, err := strconv.ParseFloat(rec.SucsfbidRate, 64); err == nil {
			awardRate = &v
		}
		var openedAt *time.Time
		if t, err := parseScsbidTime(rec.RlOpengDt); err == nil {
			openedAt = &t
		}
		var participantCount *int
		if v, err := strconv.Atoi(strings.TrimSpace(rec.PrtcptCnum)); err == nil {
			participantCount = &v
		}

		// raw_payload엔 진짜 원본 JSON을 저장한다(예전엔 공고명만 넣던 한계를
		// 2026-08-09 수정 — AwardRecord.Raw 신설). API 필드 변경 대응/재분석 근거.
		rawPayload := rec.BidNtceNm
		if len(rec.Raw) > 0 {
			rawPayload = string(rec.Raw)
		}
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO notice_award_history
				(source_id, external_bid_id, organization_name, title, winner_name, winner_bizno, award_amount, award_rate, opened_at, raw_payload, participant_count)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (source_id, external_bid_id) DO UPDATE SET
				organization_name = EXCLUDED.organization_name,
				title = EXCLUDED.title,
				winner_name = EXCLUDED.winner_name,
				winner_bizno = EXCLUDED.winner_bizno,
				award_amount = EXCLUDED.award_amount,
				award_rate = EXCLUDED.award_rate,
				opened_at = EXCLUDED.opened_at,
				raw_payload = EXCLUDED.raw_payload,
				participant_count = EXCLUDED.participant_count,
				collected_at = now()
		`, sourceID, externalBidID, orgName, nullIfEmpty(&rec.BidNtceNm), nullIfEmpty(&rec.BidwinnrNm), nullIfEmpty(&rec.BidwinnrBizno),
			awardAmount, awardRate, openedAt, rawPayload, participantCount)
		if err != nil {
			return written, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written++
		}
	}
	return written, nil
}

// ensureScsbidDataSource upserts the data_sources row for code='scsbid',
// same pattern pgstore.Open uses for the notices-collection sources.
func (s *Server) ensureScsbidDataSource(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO data_sources (code, name, source_type, base_url)
		VALUES ('scsbid', '조달청_나라장터 낙찰정보서비스', 'procurement', 'https://apis.data.go.kr/1230000/as/ScsbidInfoService')
		ON CONFLICT (code) DO UPDATE SET base_url = EXCLUDED.base_url
		RETURNING id`).Scan(&id)
	return id, err
}

// handleRunAwardHistoryIngestion manually fires the scsbid ingestion batch —
// same system_admin-only pattern as handleRunPipelineAutoTransitions/
// handleRunNotifications. Useful to trigger once right after the
// SCSBID/G2B_SERVICE_KEY access issue clears, without waiting for the next
// daily tick.
func (s *Server) handleRunAwardHistoryIngestion(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	role, err := s.userRole(r.Context(), userID)
	if err != nil {
		s.logger.Error("run-award-history-ingestion: role lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if role != "system_admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if s.scsbidSource == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scsbid_not_configured"})
		return
	}

	written, err := s.RunAwardHistoryIngestion(r.Context(), s.scsbidSource)
	if err != nil {
		s.logger.Error("run-award-history-ingestion: batch failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ingestion_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "completed", "recordsWritten": written})
}

func parseScsbidTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, strconv.ErrSyntax
	}
	return time.ParseInLocation("2006-01-02 15:04:05", v, time.Local)
}
