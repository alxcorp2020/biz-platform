package api

// notice_enrichment.go — Phase C(2026-08-11). 나라장터 추가 공식 오퍼레이션으로 공고의
// "투찰자격 제한"(참가가능지역·허용업종/면허)을 증분 보강한다.
//
// 증분(§8): enrichment_status IS NULL인 현재 버전만, 1회 사이클당 소량(enrichmentBatchSize)만
// 처리한다. 이미 보강된 버전은 다시 호출하지 않아 일일 쿼터를 낭비하지 않는다. 공고별 상세
// 페이지 로드 때 실시간 호출하지 않고(응답 지연·N+1·쿼터 방지) 미리 DB에 채워둔다.

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"time"

	"biz-platform/collector/internal/collector/sources/g2b"
)

const defaultEnrichmentBatchSize = 20 // 1 사이클당 기본 최대 공고 수(공고당 2콜 → 40콜/사이클)

// enrichmentBatchSize — 1 사이클당 처리 공고 수. 환경변수 NOTICE_ENRICHMENT_BATCH_SIZE로
// 재정의(백필 가속용, 미설정/이상값이면 기본 20). 실제 처리량은 EnrichmentClient의 일일
// 레이트리밋 캡에도 걸리므로, batch만 올리고 perDay를 안 올리면 하루 상한은 그대로다.
func enrichmentBatchSize() int {
	if v := os.Getenv("NOTICE_ENRICHMENT_BATCH_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultEnrichmentBatchSize
}

// noticeEnricher — g2b.EnrichmentClient가 구현. 테스트에서 목으로 교체 가능.
type noticeEnricher interface {
	FetchParticipationRegions(ctx context.Context, bidNtceNo, bidNtceOrd string) ([]g2b.RegionLimit, error)
	FetchLicenseLimits(ctx context.Context, bidNtceNo, bidNtceOrd string) ([]g2b.LicenseLimit, error)
}

// licenseLimitItem — 상세 응답용 허용업종/면허 1건.
type licenseLimitItem struct {
	Name     string `json:"name"`               // "폐기물종합처분업/1143"
	GroupNo  string `json:"groupNo,omitempty"`  // OR/AND 그룹 힌트(원문 값 그대로)
	Industry string `json:"industry,omitempty"` // permitted_industries(있을 때만)
}

// RunNoticeEnrichment — 미보강 현재 버전을 소량 골라 참가가능지역/허용면허를 채운다.
// 반환값은 이번 사이클에 보강 시도한 공고 수. enricher가 nil이면(키 미설정) no-op.
func (s *Server) RunNoticeEnrichment(ctx context.Context, enricher noticeEnricher) (int, error) {
	if enricher == nil {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT nv.id, n.external_notice_id, COALESCE(rd.raw_content::jsonb->>'bidNtceOrd', '')
		FROM notice_versions nv
		JOIN notices n ON n.id = nv.notice_id
		JOIN raw_documents rd ON rd.id = nv.raw_document_id
		WHERE nv.is_current = true
		  AND n.notice_type = 'procurement'
		  AND nv.enrichment_status IS NULL
		ORDER BY nv.collected_at DESC NULLS LAST
		LIMIT $1`, enrichmentBatchSize())
	if err != nil {
		return 0, err
	}
	type target struct{ versionID, bidNtceNo, bidNtceOrd string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.versionID, &t.bidNtceNo, &t.bidNtceOrd); err != nil {
			continue
		}
		if t.bidNtceNo != "" {
			targets = append(targets, t)
		}
	}
	rows.Close()

	processed := 0
	for _, t := range targets {
		processed++
		regions, rerr := enricher.FetchParticipationRegions(ctx, t.bidNtceNo, t.bidNtceOrd)
		licenses, lerr := enricher.FetchLicenseLimits(ctx, t.bidNtceNo, t.bidNtceOrd)
		if rerr != nil || lerr != nil {
			// 실패 — 'error'로 표시해 매 사이클 재시도로 쿼터를 낭비하지 않는다.
			// (요구 시 관리자가 enrichment_status를 NULL로 되돌려 재큐잉 가능.)
			s.markEnrichmentStatus(ctx, t.versionID, "error")
			s.logger.Warn("notice enrichment failed", "bidNtceNo", t.bidNtceNo, "regionErr", rerr, "licenseErr", lerr)
			continue
		}
		if err := s.saveEnrichment(ctx, t.versionID, regions, licenses); err != nil {
			s.markEnrichmentStatus(ctx, t.versionID, "error")
			s.logger.Error("notice enrichment save failed", "bidNtceNo", t.bidNtceNo, "error", err)
			continue
		}
	}
	return processed, nil
}

// saveEnrichment — 한 버전의 지역/면허를 원자적으로 교체하고 상태를 확정한다.
func (s *Server) saveEnrichment(ctx context.Context, versionID string, regions []g2b.RegionLimit, licenses []g2b.LicenseLimit) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM notice_participation_regions WHERE notice_version_id = $1`, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM notice_license_limits WHERE notice_version_id = $1`, versionID); err != nil {
		return err
	}
	ns := func(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
	for _, r := range regions {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO notice_participation_regions (notice_version_id, region_name, business_division, sort_no)
			 VALUES ($1,$2,$3,$4)`, versionID, r.RegionName, ns(r.BusinessDivision), r.SortNo); err != nil {
			return err
		}
	}
	for _, l := range licenses {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO notice_license_limits (notice_version_id, license_name, permitted_industries, industry_field, limit_group_no, business_division, sort_no)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			versionID, l.LicenseName, ns(l.PermittedIndustries), ns(l.IndustryField),
			ns(l.LimitGroupNo), ns(l.BusinessDivision), l.SortNo); err != nil {
			return err
		}
	}
	status := "completed"
	if len(regions) == 0 && len(licenses) == 0 {
		status = "not_found" // 제한 없음(정상) — 완료로 취급해 재조회 안 함
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE notice_versions SET enrichment_status = $2, enriched_at = now() WHERE id = $1`, versionID, status); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) markEnrichmentStatus(ctx context.Context, versionID, status string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE notice_versions SET enrichment_status = $2, enriched_at = now() WHERE id = $1`, versionID, status)
}

// listParticipationRegions — 상세용 참가가능지역 이름 목록(중복 제거, sort_no 순).
func (s *Server) listParticipationRegions(ctx context.Context, versionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT region_name FROM notice_participation_regions
		WHERE notice_version_id = $1 ORDER BY sort_no, region_name`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			continue
		}
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// listLicenseLimits — 상세용 허용업종/면허 목록(이름 기준 중복 제거, sort_no 순).
func (s *Server) listLicenseLimits(ctx context.Context, versionID string) ([]licenseLimitItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT license_name, COALESCE(limit_group_no,''), COALESCE(permitted_industries,'')
		FROM notice_license_limits
		WHERE notice_version_id = $1 ORDER BY sort_no, license_name`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []licenseLimitItem{}
	for rows.Next() {
		var it licenseLimitItem
		if err := rows.Scan(&it.Name, &it.GroupNo, &it.Industry); err != nil {
			continue
		}
		if it.Name == "" || seen[it.Name] {
			continue
		}
		seen[it.Name] = true
		out = append(out, it)
	}
	return out, nil
}

// TriggerNoticeEnrichmentOnView — 상세 조회 시 그 공고(procurement)가 아직 미보강이면 즉시
// 비동기로 참가가능지역/허용면허를 보강한다. 배경 sweep(15분 증분)이 아직 못 온 공고라도
// "사용자가 방금 연 공고"를 우선 채워 다음 로드에서 입찰자격이 보이게 한다.
//   - 응답을 지연시키지 않도록 완전 비동기(fire-and-forget), 같은 공고 중복 실행은 in-flight 맵으로 차단.
//   - 배경 sweep과 동일한 EnrichmentClient(rate-limit/일일쿼터 공유)를 써서 쿼터를 초과하지 않는다.
//   - 실패해도 status를 'error'로 확정하지 않는다(배경 sweep이 나중에 정식 재시도하도록 NULL 유지).
func (s *Server) TriggerNoticeEnrichmentOnView(noticeID string) {
	if s.noticeEnricher == nil || noticeID == "" {
		return
	}
	s.enrichInflightMu.Lock()
	if s.enrichInflight == nil {
		s.enrichInflight = make(map[string]bool)
	}
	if s.enrichInflight[noticeID] {
		s.enrichInflightMu.Unlock()
		return
	}
	s.enrichInflight[noticeID] = true
	s.enrichInflightMu.Unlock()

	go func() {
		defer func() {
			s.enrichInflightMu.Lock()
			delete(s.enrichInflight, noticeID)
			s.enrichInflightMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		var versionID, bidNtceNo, bidNtceOrd string
		var status sql.NullString
		err := s.db.QueryRowContext(ctx, `
			SELECT nv.id, nv.enrichment_status, n.external_notice_id,
			       COALESCE(rd.raw_content::jsonb->>'bidNtceOrd', '')
			FROM notice_versions nv
			JOIN notices n ON n.id = nv.notice_id
			JOIN raw_documents rd ON rd.id = nv.raw_document_id
			WHERE nv.notice_id = $1 AND nv.is_current = true AND n.notice_type = 'procurement'`,
			noticeID).Scan(&versionID, &status, &bidNtceNo, &bidNtceOrd)
		if err != nil {
			return // 지원사업이거나 원본 없음 → on-view 보강 대상 아님
		}
		if status.Valid && status.String != "" {
			return // 이미 completed/not_found/error → 재보강 안 함
		}
		if bidNtceNo == "" {
			return
		}
		regions, rerr := s.noticeEnricher.FetchParticipationRegions(ctx, bidNtceNo, bidNtceOrd)
		licenses, lerr := s.noticeEnricher.FetchLicenseLimits(ctx, bidNtceNo, bidNtceOrd)
		if rerr != nil || lerr != nil {
			s.logger.Warn("on-view enrichment fetch failed", "bidNtceNo", bidNtceNo, "regionErr", rerr, "licenseErr", lerr)
			return // status 유지(NULL) → 배경 sweep이 정식 재시도
		}
		if err := s.saveEnrichment(ctx, versionID, regions, licenses); err != nil {
			s.logger.Error("on-view enrichment save failed", "bidNtceNo", bidNtceNo, "error", err)
			return
		}
		s.logger.Info("on-view enrichment completed", "noticeID", noticeID, "regions", len(regions), "licenses", len(licenses))
	}()
}
