// award_history.go — 공고 상세의 "동일 발주기관 과거 낙찰 이력" +
// "경쟁 강도 참고". notice_award_history는 조달청 나라장터
// 낙찰정보서비스(ScsbidInfoService) 수집기(collector/internal/collector/
// sources/scsbid)로 실제 채워지고 있다(2026-08-06, award_history_ingest.go
// 배치). 이 파일의 조회 로직은 테이블이 비어 있어도 정상적으로 "아직
// 수집된 낙찰 이력이 없습니다" 상태를 반환한다.
//
// 🚨 업종별 통계는 이번 범위에서 제외한다(2026-08-06, 사용자 확인) —
// notice_award_history.industry 컬럼은 스키마엔 있지만 항상 NULL이다.
// scsbid API(getScsbidListSttusServcPPSSrch) 응답 자체에 업종 분류
// 필드가 없어서 수집 코드가 애초에 채운 적이 없다(억지로 notices.industry와
// 이름 매칭해서 유추하는 방법도 있지만 신뢰도가 낮아 이번 범위 밖 —
// "데이터가 적은 초기 플랫폼" 원칙: 계산 불가능하면 억지로 만들지
// 않는다). 발주기관별 통계(지역 개념)만 이 파일에서 다룬다.
//
// 최소 표본 게이트(AWARD_HISTORY_MIN_SAMPLE=5, index.html)도 같은
// 원칙 — 발주기관 172곳 중 5건 이상은 4곳뿐(2026-08-06 실측)이라
// 이 백엔드는 표본 수와 무관하게 항상 전체 데이터를 내려주고, "몇 건
// 미만이면 안 보여준다"는 판단은 프론트가 한다(백엔드는 있는 그대로의
// 사실만 전달, 표시 정책은 화면 쪽 책임으로 분리).
package api

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"
)

type awardHistoryItem struct {
	Title            string   `json:"title"`
	WinnerName       *string  `json:"winnerName"`
	AwardAmount      *int64   `json:"awardAmount"`
	AwardRate        *float64 `json:"awardRate"`
	OpenedAt         *string  `json:"openedAt"`
	ParticipantCount *int     `json:"participantCount"`
}

// frequentWinnerItem — "자주 낙찰받는 업체"(경쟁사 파악용, 2026-08-06 추가).
type frequentWinnerItem struct {
	Name     string `json:"name"`
	WinCount int    `json:"winCount"`
}

// rateTrendPoint — 낙찰률 추이용 개별 데이터 포인트. 월별/주별로 묶지
// 않고 개별 낙찰 건을 시간순 그대로 내려준다 — scsbid 수집이 이제 막
// 시작돼(2026-08-06) 발주기관 하나당 표본이 아직 적어, 기간별로 묶으면
// 버킷 하나에 1~2건만 들어가는 경우가 흔해 오히려 오해를 부를 수 있다.
// 프론트가 2건 미만이면(성장분석 페이지의 기존 라인차트 표시 기준과
// 동일) 그래프 대신 문구로 대체한다.
type rateTrendPoint struct {
	Date string  `json:"date"`
	Rate float64 `json:"rate"`
}

type organizationAwardHistoryDTO struct {
	Count               int                  `json:"count"`
	AverageRate         *float64             `json:"averageRate"`
	AverageParticipants *float64             `json:"averageParticipants"`
	Items               []awardHistoryItem   `json:"items"`
	FrequentWinners     []frequentWinnerItem `json:"frequentWinners"`
	RateTrend           []rateTrendPoint     `json:"rateTrend"`
}

// fetchOrganizationAwardHistory returns up to 10 most recent award records
// matching this notice's agency, plus the average award rate across ALL
// matching records (not just the 10 shown) — averaging over a small display
// window would be misleading.
//
// It matches against BOTH organizationName(공고기관명)과 departmentName
// (수요기관명), not organizationName alone. 이유: notice_award_history를
// 채우는 scsbid 수집기(collector/internal/collector/sources/scsbid)는
// getScsbidListSttusServcPPSSrch 응답에 수요기관명(dminsttNm)만 있고
// 공고기관명(ntceInsttNm)은 없어서, organization_name 컬럼에 dminsttNm
// 값을 저장한다. 조달청이 공고기관으로 대행하는 공고는 전부 organization_
// name="조달청"으로 동일해, notices.organization_name(마찬가지로
// ntceInsttNm 기반)만으로 매칭하면 조달청-대행 공고 전부가 서로 매칭되는
// 무의미한 결과가 나온다 — notices.department_name(dminsttNm 기반)까지
// 같이 매칭해야 실제 같은 수요기관 이력을 잡아낸다.
//
// organizationName/departmentName 둘 다 빈 문자열이면(공고에 기관명이
// 전혀 없는 경우) nil, nil을 반환 — 호출부가 섹션 자체를 생략한다.
func (s *Server) fetchOrganizationAwardHistory(ctx context.Context, organizationName, departmentName string) (*organizationAwardHistoryDTO, error) {
	names := dedupNonEmpty(organizationName, departmentName)
	if len(names) == 0 {
		return nil, nil
	}

	var dto organizationAwardHistoryDTO
	var avgRate, avgParticipants sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), AVG(award_rate), AVG(participant_count) FROM notice_award_history WHERE organization_name = ANY($1)
	`, pq.Array(names)).Scan(&dto.Count, &avgRate, &avgParticipants); err != nil {
		return nil, err
	}
	dto.Items = []awardHistoryItem{}
	dto.FrequentWinners = []frequentWinnerItem{}
	dto.RateTrend = []rateTrendPoint{}
	if dto.Count == 0 {
		return &dto, nil
	}
	if avgRate.Valid {
		dto.AverageRate = &avgRate.Float64
	}
	if avgParticipants.Valid {
		dto.AverageParticipants = &avgParticipants.Float64
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT title, winner_name, award_amount, award_rate, opened_at, participant_count
		FROM notice_award_history
		WHERE organization_name = ANY($1)
		ORDER BY opened_at DESC NULLS LAST
		LIMIT 10`, pq.Array(names))
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
		var participants sql.NullInt64
		if err := rows.Scan(&title, &winner, &amount, &rate, &opened, &participants); err != nil {
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
		if participants.Valid {
			v := int(participants.Int64)
			it.ParticipantCount = &v
		}
		dto.Items = append(dto.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frequentWinners, err := s.fetchFrequentAwardWinners(ctx, names)
	if err != nil {
		return nil, err
	}
	dto.FrequentWinners = frequentWinners

	rateTrend, err := s.fetchAwardRateTrend(ctx, names)
	if err != nil {
		return nil, err
	}
	dto.RateTrend = rateTrend

	return &dto, nil
}

// fetchFrequentAwardWinners — "자주 낙찰받는 업체"(경쟁사 파악용). 낙찰
// 횟수 기준 상위 5개사만 보여준다(전체 목록은 위 items 표에서 개별
// 확인 가능하므로, 여긴 "반복해서 이기는 곳이 누구인지"만 빠르게
// 보여주는 요약).
func (s *Server) fetchFrequentAwardWinners(ctx context.Context, names []string) ([]frequentWinnerItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT winner_name, COUNT(*) AS win_count
		FROM notice_award_history
		WHERE organization_name = ANY($1) AND winner_name IS NOT NULL AND winner_name != ''
		GROUP BY winner_name
		ORDER BY win_count DESC, MAX(opened_at) DESC NULLS LAST
		LIMIT 5`, pq.Array(names))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []frequentWinnerItem{}
	for rows.Next() {
		var it frequentWinnerItem
		if err := rows.Scan(&it.Name, &it.WinCount); err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// fetchAwardRateTrend returns up to the 20 most recent award-rate data
// points in chronological order(오래된 것부터) — 프론트가 그대로 라인
// 그래프의 x축 순서로 쓸 수 있게.
func (s *Server) fetchAwardRateTrend(ctx context.Context, names []string) ([]rateTrendPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT award_rate, opened_at FROM (
			SELECT award_rate, opened_at
			FROM notice_award_history
			WHERE organization_name = ANY($1) AND award_rate IS NOT NULL AND opened_at IS NOT NULL
			ORDER BY opened_at DESC
			LIMIT 20
		) recent
		ORDER BY opened_at ASC`, pq.Array(names))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []rateTrendPoint{}
	for rows.Next() {
		var rate float64
		var opened time.Time
		if err := rows.Scan(&rate, &opened); err != nil {
			continue
		}
		out = append(out, rateTrendPoint{Date: opened.Format("2006-01-02"), Rate: rate})
	}
	return out, rows.Err()
}

// dedupNonEmpty returns the distinct non-empty strings among names,
// preserving order — used to build an IN-list without duplicating a name
// that happens to equal itself (e.g. organizationName == departmentName).
func dedupNonEmpty(names ...string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
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
