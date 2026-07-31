// award_history.go — 공고 상세의 "동일 발주기관 과거 낙찰 이력" +
// "경쟁 강도 참고". notice_award_history는 조달청 나라장터
// 낙찰정보서비스(ScsbidInfoService) 연동으로 채워질 예정이지만, 그
// 수집기는 API 활용신청 승인 후 별도로 추가한다(collector/internal/
// collector/sources/scsbid — 아직 없음). 이 파일의 조회 로직은 테이블이
// 비어 있어도 정상적으로 "아직 수집된 낙찰 이력이 없습니다" 상태를
// 반환하도록 만들어졌다 — 수집기가 나중에 붙어도 이쪽은 손댈 필요 없음.
package api

import (
	"context"
	"database/sql"
)

type awardHistoryItem struct {
	Title       string   `json:"title"`
	WinnerName  *string  `json:"winnerName"`
	AwardAmount *int64   `json:"awardAmount"`
	AwardRate   *float64 `json:"awardRate"`
	OpenedAt    *string  `json:"openedAt"`
}

type organizationAwardHistoryDTO struct {
	Count       int                `json:"count"`
	AverageRate *float64           `json:"averageRate"`
	Items       []awardHistoryItem `json:"items"`
}

// fetchOrganizationAwardHistory returns up to 10 most recent award records
// for organizationName, plus the average award rate across ALL matching
// records (not just the 10 shown) — averaging over a small display window
// would be misleading. organizationName == "" (공고에 발주기관명이 없는
// 경우) returns nil, nil — 호출부가 섹션 자체를 생략한다.
func (s *Server) fetchOrganizationAwardHistory(ctx context.Context, organizationName string) (*organizationAwardHistoryDTO, error) {
	if organizationName == "" {
		return nil, nil
	}

	var dto organizationAwardHistoryDTO
	var avgRate sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), AVG(award_rate) FROM notice_award_history WHERE organization_name = $1
	`, organizationName).Scan(&dto.Count, &avgRate); err != nil {
		return nil, err
	}
	dto.Items = []awardHistoryItem{}
	if dto.Count == 0 {
		return &dto, nil
	}
	if avgRate.Valid {
		dto.AverageRate = &avgRate.Float64
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT title, winner_name, award_amount, award_rate, opened_at
		FROM notice_award_history
		WHERE organization_name = $1
		ORDER BY opened_at DESC NULLS LAST
		LIMIT 10`, organizationName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var it awardHistoryItem
		var title, winner sql.NullString
		var amount sql.NullInt64
		var rate sql.NullFloat64
		var opened sql.NullTime
		if err := rows.Scan(&title, &winner, &amount, &rate, &opened); err != nil {
			continue
		}
		it.Title = title.String
		it.WinnerName = nullStringPtr(winner)
		if amount.Valid {
			it.AwardAmount = &amount.Int64
		}
		if rate.Valid {
			it.AwardRate = &rate.Float64
		}
		if opened.Valid {
			v := opened.Time.Format("2006-01-02")
			it.OpenedAt = &v
		}
		dto.Items = append(dto.Items, it)
	}
	return &dto, rows.Err()
}

// hasTrackRecordOverlap reports whether this company's own 수행실적
// (company_track_records) includes a project for the same client
// (organization_name) or the same industry_field as this notice — a
// lightweight signal for "경쟁 강도 참고"(우리 회사도 이 분야/발주처에서
// 뛰고 있다 = 경쟁사도 비슷한 이력을 가졌을 가능성이 높다는 맥락 정보일
// 뿐, 수치화된 점수가 아니다 — 스펙에서 "개별 경쟁사 프로필 비교는 범위
// 밖"이라 명시했으므로 딱 이 정도 참고 표시로 그친다).
func (s *Server) hasTrackRecordOverlap(ctx context.Context, profileID, organizationName, industry string) (bool, error) {
	if profileID == "" || (organizationName == "" && industry == "") {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM company_track_records
			WHERE company_profile_id = $1
			  AND ((client_name = $2 AND $2 != '') OR (industry_field = $3 AND $3 != ''))
		)`, profileID, organizationName, industry,
	).Scan(&exists)
	return exists, err
}
