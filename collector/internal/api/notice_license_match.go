// notice_license_match.go — 판정엔진 확장 3단계(2026-08-07): 공고 상세의
// "참가조건" 탭에서, 이 공고가 요구하는 면허·인증과 회사가 실제로 보유한
// 면허·인증(company_licenses/company_certifications)을 대조해 구체적인
// 안내 문장을 만든다.
//
// 데이터 출처는 required_documents.document_name이다 — g2b 원문에는
// "이 공고가 요구하는 면허명" 같은 구조화된 필드가 없고(업종제한 여부
// 불리언만 있음, renderNoticeDetailInfo 참고), 문서 AI 분석(document_extraction.go)이
// 첨부파일에서 추출한 서류명이 유일한 실제 데이터다. dashboard.go의
// documentRequirementCategories(온보딩 카드 큐)가 이미 이 필드에서 면허/
// 인증 키워드를 걸러내는 패턴을 검증해뒀으므로 그 키워드를 그대로
// 재사용한다("인증"(2글자)이 "확인증명서" 안에 우연히 포함돼 오탐되는
// 문제를 이미 겪어서 "면허증"/"인증서"/"ISO"로 좁혀놓은 것).
//
// 매칭은 company_pipeline.go의 matchChecklistStatus와 동일하게 TRIM한
// 정확 일치만 본다(유사도 스코어링 없음) — 애매하면 "확인 필요"로 정직하게
// 남긴다.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

type licenseRequirementMatch struct {
	DocumentName string `json:"documentName"`
	Held         bool   `json:"held"`
	Message      string `json:"message"`
}

// licenseCertKeywords — documentRequirementCategories(dashboard.go)의
// license/certification 카테고리와 동일한 키워드.
var licenseCertKeywords = []string{"%면허증%", "%인증서%", "%ISO%"}

// matchNoticeLicenseRequirements returns one entry per distinct
// license/certification-looking required_documents row for this notice
// version, each flagged against whether the company holds a same-named
// (TRIM'd exact match) license or certification with status='보유'.
func (s *Server) matchNoticeLicenseRequirements(ctx context.Context, versionID, profileID string) ([]licenseRequirementMatch, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT document_name FROM required_documents
		WHERE notice_version_id = $1 AND review_status != 'rejected' AND document_name ILIKE ANY($2)`,
		versionID, pq.Array(licenseCertKeywords),
	)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			continue
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]licenseRequirementMatch, 0, len(names))
	for _, docName := range names {
		name := strings.TrimSpace(docName)
		if name == "" {
			continue
		}
		var status string
		err := s.db.QueryRowContext(ctx, `
			SELECT status FROM (
				SELECT status, created_at FROM company_licenses WHERE company_profile_id = $1 AND TRIM(name) = $2
				UNION ALL
				SELECT status, created_at FROM company_certifications WHERE company_profile_id = $1 AND TRIM(name) = $2
			) matched ORDER BY created_at DESC LIMIT 1`,
			profileID, name,
		).Scan(&status)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		held := status == "보유"
		var msg string
		if held {
			msg = fmt.Sprintf("보유하신 '%s'이(가) 이 공고 요건과 일치합니다.", name)
		} else {
			msg = fmt.Sprintf("이 공고는 '%s'을(를) 요구하는데 등록된 면허가 없습니다 — 확인 필요.", name)
		}
		out = append(out, licenseRequirementMatch{DocumentName: name, Held: held, Message: msg})
	}
	return out, nil
}
