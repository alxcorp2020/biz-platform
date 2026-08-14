// notice_requirements.go — 공고 요구조건 Requirement Resolver (STEP 1, 2026-08-14).
//
// 여러 원본 소스(notice_license_limits / notice_participation_regions / required_documents /
// eligibility_conditions)에 흩어진 공고 참가 요구조건을 판정기가 각자 직접 읽지 않도록,
// 하나의 얇은 read/normalize 계층으로 합친다. 신규 테이블·신규 AI 엔진을 만들지 않고 기존
// 저장분만 읽는다. 근거(source/source_text/confidence)를 보존하고, 같은 조건이 여러 소스에
// 있으면 우선순위 높은 소스로 합치며(중복 제거), 확인 불가한 것은 UNKNOWN(요구조건 미생성)로
// 남긴다 — 원문 근거 없이 요구조건을 추측 생성하지 않는다.
//
// ⚠️ 의미 주의: notice_license_limits는 "허용면허(참가가능 업종/면허, OR 범위)"라 "반드시
// 보유해야 하는 필수 면허"와 의미가 다르다. 그래서 Resolver 출력에는 Source="license_limits"로
// 포함하되(투명성), 실제 참가자격 판정(buildParticipationJudgment)에서 개별 HARD 면허요건으로
// 소비하지는 않는다(judgmentConsumableRequirements 참고 — required_documents/eligibility_conditions만).
package api

import (
	"context"
	"sort"
	"strings"
)

const (
	reqTypeRegion           = "REGION"
	reqTypeIndustry         = "INDUSTRY"
	reqTypeLicense          = "LICENSE"
	reqTypeCertification    = "CERTIFICATION"
	reqTypeDirectProduction = "DIRECT_PRODUCTION"
	reqTypeCompanySize      = "COMPANY_SIZE"
	reqTypeTrackRecord      = "TRACK_RECORD"
)

// noticeRequirement — 정규화된 단일 요구조건.
type noticeRequirement struct {
	Type          string   `json:"type"`
	NormalizedKey string   `json:"normalizedKey"` // 중복 제거 키(표기 변형 통합)
	DisplayName   string   `json:"displayName"`   // 화면·판정용 원본 이름
	Mandatory     *bool    `json:"mandatory,omitempty"`
	Source        string   `json:"source"`               // license_limits|participation_regions|required_documents|eligibility_conditions
	SourceText    string   `json:"sourceText,omitempty"` // 원문 근거
	Confidence    *float64 `json:"confidence,omitempty"`
	// 정량 비교용(주로 TRACK_RECORD 실적 요구에서 사용) — eligibility_conditions에만
	// 존재. 없으면 빈 문자열. 판정기가 company_track_records와 대조할 때만 참고한다.
	Operator       string `json:"operator,omitempty"`
	ThresholdValue string `json:"thresholdValue,omitempty"`
	Unit           string `json:"unit,omitempty"`
}

// reqSourcePriority — 낮을수록 우선. 같은 정규화 키가 여러 소스에 있으면 우선순위 높은 소스로 합친다.
var reqSourcePriority = map[string]int{
	"eligibility_conditions": 0, // 명시적 구조화 조건(근거·confidence 있음)
	"license_limits":         1, // g2b 전용 enrichment(공식 허용면허)
	"participation_regions":  1,
	"required_documents":     2, // 제출서류명 기반(추론적)
}

// normalizeRequirementName — 면허/인증명 중복 제거용 키. TRIM + 공백 제거 + 흔한 문서 접미어
// 1개 제거로 "정보통신공사업 / 정보통신공사업 면허 / 정보통신공사업 면허증 / 정보통신공사업
// 등록증"을 한 키로 모은다(표기 변형 통합일 뿐 의미 추측이 아님). 결과가 비면 원본 유지.
// 표시명(DisplayName)은 원본을 유지하고 이 키는 dedup에만 쓴다.
func normalizeRequirementName(s string) string {
	base := strings.Join(strings.Fields(strings.TrimSpace(s)), "")
	if base == "" {
		return ""
	}
	for _, suf := range []string{"증명서", "등록증", "신고증", "면허증", "인증서", "면허", "사본"} {
		if strings.HasSuffix(base, suf) {
			trimmed := strings.TrimSuffix(base, suf)
			if len([]rune(trimmed)) >= 2 { // 과다 절삭 방지(예: "인증서" 단독 → 유지)
				return trimmed
			}
		}
	}
	return base
}

func mapEligibilityCategoryToReqType(cat string) string {
	switch strings.TrimSpace(cat) {
	case "지역":
		return reqTypeRegion
	case "업종":
		return reqTypeIndustry
	case "면허":
		return reqTypeLicense
	case "인증":
		return reqTypeCertification
	case "직접생산":
		return reqTypeDirectProduction
	case "실적":
		return reqTypeTrackRecord
	case "기업규모", "예산", "매출", "예산 규모":
		return reqTypeCompanySize
	}
	return ""
}

// resolveNoticeRequirements — versionID의 요구조건을 정규화해 반환한다. versionID=="" 이면
// 구조화 소스(license_limits/participation_regions/eligibility_conditions)는 건너뛰고 이미
// 로드된 requiredDocs만 정규화한다(대시보드 추천 카드 경로 호환). 모든 쿼리는 실패해도
// 로그만 남기고 계속한다(부분 실패가 판정 전체를 막지 않음).
func (s *Server) resolveNoticeRequirements(ctx context.Context, versionID string, requiredDocs []requiredDocumentItem) []noticeRequirement {
	byKey := map[string]noticeRequirement{}
	add := func(req noticeRequirement) {
		if req.NormalizedKey == "" {
			return
		}
		k := req.Type + "|" + req.NormalizedKey
		existing, ok := byKey[k]
		if !ok {
			byKey[k] = req
			return
		}
		// 우선순위 높은 소스로 대체(근거 보존). mandatory=true는 어느 소스든 유지.
		mandatory := existing.Mandatory
		if req.Mandatory != nil && *req.Mandatory {
			mandatory = req.Mandatory
		}
		if reqSourcePriority[req.Source] < reqSourcePriority[existing.Source] {
			req.Mandatory = mandatory
			byKey[k] = req
		} else {
			existing.Mandatory = mandatory
			byKey[k] = existing
		}
	}

	if versionID != "" {
		if rows, err := s.db.QueryContext(ctx, `SELECT license_name FROM notice_license_limits WHERE notice_version_id = $1`, versionID); err == nil {
			for rows.Next() {
				var name string
				if rows.Scan(&name) == nil {
					if name = strings.TrimSpace(name); name != "" {
						m := true
						add(noticeRequirement{Type: reqTypeLicense, NormalizedKey: normalizeRequirementName(name), DisplayName: name, Mandatory: &m, Source: "license_limits", SourceText: "공식 허용면허/업종"})
					}
				}
			}
			rows.Close()
		} else {
			s.logger.Error("resolver: license_limits query failed", "error", err)
		}

		if rows, err := s.db.QueryContext(ctx, `SELECT region_name FROM notice_participation_regions WHERE notice_version_id = $1`, versionID); err == nil {
			for rows.Next() {
				var name string
				if rows.Scan(&name) == nil {
					if name = strings.TrimSpace(name); name != "" {
						m := true
						add(noticeRequirement{Type: reqTypeRegion, NormalizedKey: name, DisplayName: name, Mandatory: &m, Source: "participation_regions", SourceText: "공식 참가가능지역"})
					}
				}
			}
			rows.Close()
		} else {
			s.logger.Error("resolver: participation_regions query failed", "error", err)
		}

		if rows, err := s.db.QueryContext(ctx, `SELECT category, condition_name, is_required, COALESCE(source_text,''), confidence, COALESCE(operator,''), COALESCE(threshold_value,''), COALESCE(unit,'') FROM eligibility_conditions WHERE notice_version_id = $1 AND review_status <> 'rejected'`, versionID); err == nil {
			for rows.Next() {
				var cat, cname, src, op, thr, unit string
				var isReq bool
				var conf float64
				if rows.Scan(&cat, &cname, &isReq, &src, &conf, &op, &thr, &unit) == nil {
					if t := mapEligibilityCategoryToReqType(cat); t != "" {
						name := strings.TrimSpace(cname)
						key := normalizeRequirementName(name)
						if key == "" {
							key = t // 이름 없는 조건(지역/규모 등)은 타입 자체를 키로
						}
						m := isReq
						c := conf
						add(noticeRequirement{Type: t, NormalizedKey: key, DisplayName: name, Mandatory: &m, Source: "eligibility_conditions", SourceText: src, Confidence: &c,
							Operator: strings.TrimSpace(op), ThresholdValue: strings.TrimSpace(thr), Unit: strings.TrimSpace(unit)})
					}
				}
			}
			rows.Close()
		} else {
			s.logger.Error("resolver: eligibility_conditions query failed", "error", err)
		}
	}

	// required_documents(제출서류명) — 면허/인증/직접생산 키워드 분류. versionID 유무와 무관하게
	// 이미 로드된 파라미터를 쓴다(기존 buildParticipationJudgment 인라인 로직과 동일 규칙).
	for _, d := range requiredDocs {
		n := strings.TrimSpace(d.DocumentName)
		if n == "" {
			continue
		}
		switch {
		case strings.Contains(n, "직접생산"):
			add(noticeRequirement{Type: reqTypeDirectProduction, NormalizedKey: "직접생산확인", DisplayName: n, Source: "required_documents", SourceText: n})
		case strings.Contains(n, "실적"):
			// "실적증명서/유사용역 실적확인서" 등 제출서류명으로 실적 요구를 감지한다.
			// 정량 임계치(금액·기간)는 서류명만으로 알 수 없어 대조는 REVIEW로만 처리한다.
			add(noticeRequirement{Type: reqTypeTrackRecord, NormalizedKey: reqTypeTrackRecord, DisplayName: n, Source: "required_documents", SourceText: n})
		case strings.Contains(n, "면허"):
			add(noticeRequirement{Type: reqTypeLicense, NormalizedKey: normalizeRequirementName(n), DisplayName: n, Source: "required_documents", SourceText: n})
		case strings.Contains(n, "인증서") || strings.Contains(n, "ISO"):
			add(noticeRequirement{Type: reqTypeCertification, NormalizedKey: normalizeRequirementName(n), DisplayName: n, Source: "required_documents", SourceText: n})
		}
	}

	out := make([]noticeRequirement, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].NormalizedKey < out[j].NormalizedKey
	})
	return out
}

// judgmentConsumableRequirements — 참가자격 판정(buildParticipationJudgment)이 개별 HARD 요건으로
// 소비해도 되는 소스만 남긴다. license_limits(허용/OR 범위) 및 participation_regions(지역은
// scoreRegion이 별도 처리)는 제외한다 — 기존 판정 동작 보존(§8 regression 방지).
func judgmentConsumableRequirements(reqs []noticeRequirement) []noticeRequirement {
	out := reqs[:0:0]
	for _, r := range reqs {
		if r.Source == "required_documents" || r.Source == "eligibility_conditions" {
			out = append(out, r)
		}
	}
	return out
}
