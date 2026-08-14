// notice_detail.go — GET /api/notices/{id} 응답에 g2b 원본 JSON
// (raw_documents.raw_content)에서 뽑은 상세 필드와 첨부파일 목록을 얹어준다.
// notices 테이블에 컬럼을 계속 추가하는 대신, 필터/정렬에 쓰이지 않고
// 상세 화면 표시 전용인 필드들은 조회 시점에 그때그때 파싱한다.
//
// 주의: g2bRawFields는 g2b(나라장터) 응답 JSON 구조 전용이다. 현재 실제
// 출처가 g2b뿐이라 범위를 좁혔다 — 다른 출처가 추가되면 출처별 분기가
// 필요하다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type g2bRawFields struct {
	NtceKindNm               string `json:"ntceKindNm"`
	CntrctCnclsMthdNm        string `json:"cntrctCnclsMthdNm"`
	SucsfbidMthdNm           string `json:"sucsfbidMthdNm"`
	SucsfbidMthdAppStd       string `json:"sucsfbidMthdAppStd"`
	SucsfbidLwltRate         string `json:"sucsfbidLwltRate"`
	RbidPermsnYn             string `json:"rbidPermsnYn"`
	BidQlfctRgstDt           string `json:"bidQlfctRgstDt"`
	BidBeginDt               string `json:"bidBeginDt"`
	BidClseDt                string `json:"bidClseDt"`
	OpengDt                  string `json:"opengDt"`
	PresmptPrce              string `json:"presmptPrce"`
	VAT                      string `json:"VAT"`
	NtceInsttOfclNm          string `json:"ntceInsttOfclNm"`
	NtceInsttOfclTelNo       string `json:"ntceInsttOfclTelNo"`
	IndstrytyLmtYn           string `json:"indstrytyLmtYn"`
	RgnLmtBidLocplcJdgmBssNm string `json:"rgnLmtBidLocplcJdgmBssNm"`
	// Phase B(2026-08-11) — raw_content에 이미 있었으나 그동안 안 읽던 필드. 새 API 호출 없음.
	// 필드 의미는 실측 응답으로 확인된 것만(추측 금지). 값 없으면 프론트가 행을 안 만든다.
	BidNtceNo             string `json:"bidNtceNo"`             // 입찰공고번호
	BidNtceOrd            string `json:"bidNtceOrd"`            // 차수
	RefNo                 string `json:"refNo"`                 // 참조번호
	UntyNtceNo            string `json:"untyNtceNo"`            // 통합공고번호
	SrvceDivNm            string `json:"srvceDivNm"`            // 용역구분(일반용역 등)
	BidNtceDt             string `json:"bidNtceDt"`             // 공고 게시일시(초 단위)
	BidMethdNm            string `json:"bidMethdNm"`            // 입찰방식(전자입찰 등)
	IntrbidYn             string `json:"intrbidYn"`             // 국제입찰 여부 Y/N
	PrearngPrceDcsnMthdNm string `json:"prearngPrceDcsnMthdNm"` // 예정가격 결정방법(복수예가 등)
	DrwtPrdprcNum         string `json:"drwtPrdprcNum"`         // 예정가격 추첨 개수
	TotPrdprcNum          string `json:"totPrdprcNum"`          // 예비가격 전체 개수
	CmmnSpldmdMethdNm     string `json:"cmmnSpldmdMethdNm"`     // 공동수급 방식(불허 등)
	OpengPlce             string `json:"opengPlce"`             // 개찰 장소
	RbidOpengDt           string `json:"rbidOpengDt"`           // 재입찰 개찰일시
	AsignBdgtAmt          string `json:"asignBdgtAmt"`          // 배정예산액
	DminsttNm             string `json:"dminsttNm"`             // 수요기관명
	NtceInsttNm           string `json:"ntceInsttNm"`           // 공고기관명
	NtceInsttOfclEmailAdrs string `json:"ntceInsttOfclEmailAdrs"` // 공고기관 담당자 이메일
	DcmtgOprtnDt          string `json:"dcmtgOprtnDt"`          // 설명회 일시
	DcmtgOprtnPlce        string `json:"dcmtgOprtnPlce"`        // 설명회 장소
	BidPrtcptFee          string `json:"bidPrtcptFee"`          // 입찰참가수수료(원)
	BidGrntymnyPaymntYn   string `json:"bidGrntymnyPaymntYn"`   // 입찰보증금 납부 여부 Y/N
	// Phase B 확장(2026-08-13) — 원문 대비 누락 필드 보강. 실측 raw_content에 존재 확인된 것만.
	ReNtceYn                    string `json:"reNtceYn"`                    // 재공고 여부 Y/N
	ArsltCmptYn                 string `json:"arsltCmptYn"`                 // 실적심사(경쟁) 여부 Y/N
	ArsltApplDocRcptMthdNm      string `json:"arsltApplDocRcptMthdNm"`      // 실적신청서 제출방법(없음 등)
	PrdctClsfcLmtYn             string `json:"prdctClsfcLmtYn"`             // 물품분류 제한 여부 Y/N
	CmmnSpldmdCorpRgnLmtYn      string `json:"cmmnSpldmdCorpRgnLmtYn"`      // 공동수급체 구성원 지역제한 Y/N
	CmmnSpldmdAgrmntRcptdocMthd string `json:"cmmnSpldmdAgrmntRcptdocMethd"` // 공동수급 협정서 접수방법(수기 등)
	ExctvNm                     string `json:"exctvNm"`                     // 집행관
	OrderPlanUntyNo             string `json:"orderPlanUntyNo"`             // 발주계획 통합번호
	BfSpecRgstNo                string `json:"bfSpecRgstNo"`                // 사전규격 등록번호
	CrdtrNm                     string `json:"crdtrNm"`                     // 발주처(채권자)
	PubPrcrmntLrgClsfcNm        string `json:"pubPrcrmntLrgClsfcNm"`        // 조달 대분류
	PubPrcrmntMidClsfcNm        string `json:"pubPrcrmntMidClsfcNm"`        // 조달 중분류
	PubPrcrmntClsfcNm           string `json:"pubPrcrmntClsfcNm"`           // 조달 세부분류
}

type noticeRawDetail struct {
	NoticeKind              string     `json:"noticeKind"`
	ContractMethod          string     `json:"contractMethod"`
	AwardMethod             string     `json:"awardMethod"`
	AwardMethodDetail       string     `json:"awardMethodDetail"`
	AwardLowerLimitRate     string     `json:"awardLowerLimitRate"`
	RebidAllowed            *bool      `json:"rebidAllowed"`
	QualificationDeadlineAt *time.Time `json:"qualificationDeadlineAt"`
	ApplicationStartAt      *time.Time `json:"applicationStartAt"`
	ApplicationEndAt        *time.Time `json:"applicationEndAt"`
	BidOpeningAt            *time.Time `json:"bidOpeningAt"`
	EstimatedPrice          *int64     `json:"estimatedPrice"`
	Vat                     *int64     `json:"vat"`
	OfficerName             string     `json:"officerName"`
	OfficerPhone            string     `json:"officerPhone"`
	IndustryRestricted      *bool      `json:"industryRestricted"`
	RegionRestrictionBasis  string     `json:"regionRestrictionBasis"`
	// Phase B — raw_content에서 살린 추가 필드(새 API 호출 없음). 값 없으면 nil/빈문자열.
	NoticeNo               string     `json:"noticeNo"`
	NoticeOrd              string     `json:"noticeOrd"`
	ReferenceNo            string     `json:"referenceNo"`
	UnifiedNoticeNo        string     `json:"unifiedNoticeNo"`
	ServiceDivision        string     `json:"serviceDivision"`
	PublishedAt            *time.Time `json:"publishedAt"`
	BidMethod              string     `json:"bidMethod"`
	InternationalBid       *bool      `json:"internationalBid"`
	PrearrangedPriceMethod string     `json:"prearrangedPriceMethod"`
	PriceDrawCount         *int64     `json:"priceDrawCount"`
	PriceTotalCount        *int64     `json:"priceTotalCount"`
	JointContractMethod    string     `json:"jointContractMethod"`
	OpeningPlace           string     `json:"openingPlace"`
	RebidOpeningAt         *time.Time `json:"rebidOpeningAt"`
	AssignedBudget         *int64     `json:"assignedBudget"`
	DemandInstitution      string     `json:"demandInstitution"`
	NoticeInstitution      string     `json:"noticeInstitution"`
	OfficerEmail           string     `json:"officerEmail"`
	BriefingAt             *time.Time `json:"briefingAt"`
	BriefingPlace          string     `json:"briefingPlace"`
	ParticipationFee       *int64     `json:"participationFee"`
	BidGuaranteeRequired   *bool      `json:"bidGuaranteeRequired"`
	// Phase B 확장(2026-08-13) — 원문 누락 보강 필드.
	ReNotice               *bool  `json:"reNotice"`               // 재공고 여부
	PerformanceReview      *bool  `json:"performanceReview"`      // 실적심사(경쟁) 여부
	PerformanceDocMethod   string `json:"performanceDocMethod"`   // 실적신청서 제출방법
	ProductClassRestricted *bool  `json:"productClassRestricted"` // 물품분류 제한 여부
	JointRegionRestricted  *bool  `json:"jointRegionRestricted"`  // 공동수급체 지역제한 여부
	JointAgreementMethod   string `json:"jointAgreementMethod"`   // 공동수급 협정서 접수방법
	Executive              string `json:"executive"`              // 집행관
	OrderPlanNo            string `json:"orderPlanNo"`            // 발주계획번호
	PreSpecRegNo           string `json:"preSpecRegNo"`           // 사전규격 등록번호
	Creditor               string `json:"creditor"`               // 발주처(채권자)
	ProcurementClass       string `json:"procurementClass"`       // 조달분류(대>중>세)
	// 담당자 개인정보 마스킹(2026-08-11). OfficerName/Phone/Email에는 "표시용" 값(마스킹 또는
	// 원본)을 담는다. OfficerMasked=true면 마스킹된 상태. OfficerCanReveal=true면(참여검토 시작한
	// 사용자) 프론트에 [공개] 버튼을 띄우고 OfficerFull*로 즉시 원본 전환한다. system_admin은
	// 마스킹 없이(OfficerMasked=false) 원본을 그대로 받는다. 미인증/미참여자는 Full* 미전송.
	OfficerMasked    bool   `json:"officerMasked"`
	OfficerCanReveal bool   `json:"officerCanReveal"`
	OfficerFullName  string `json:"officerFullName,omitempty"`
	OfficerFullPhone string `json:"officerFullPhone,omitempty"`
	OfficerFullEmail string `json:"officerFullEmail,omitempty"`
}

// ---------- 담당자 개인정보 마스킹 ----------

// maskOfficerName — 성만 노출, 나머지 *. "홍길동"→"홍**", 1자 이하/빈값은 그대로.
func maskOfficerName(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= 1 {
		return string(r)
	}
	return string(r[0]) + strings.Repeat("*", len(r)-1)
}

// maskOfficerPhone — 국번(첫 그룹)만 노출, 나머지 그룹은 자릿수만큼 *.
// "062-1234-5678"→"062-****-****". 하이픈 없으면 앞 3자리만 노출.
func maskOfficerPhone(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "-") {
		parts := strings.Split(s, "-")
		for i := 1; i < len(parts); i++ {
			parts[i] = strings.Repeat("*", len([]rune(parts[i])))
		}
		return strings.Join(parts, "-")
	}
	r := []rune(s)
	if len(r) <= 3 {
		return s
	}
	return string(r[:3]) + strings.Repeat("*", len(r)-3)
}

// maskOfficerEmail — @ 앞부분(local) 앞 2글자만 노출, 나머지 local은 *. 도메인은 그대로.
// "test@example.com"→"te**@example.com". local 2자 이하/@ 없으면 그대로.
func maskOfficerEmail(s string) string {
	s = strings.TrimSpace(s)
	at := strings.Index(s, "@")
	if at <= 0 {
		return s
	}
	local := []rune(s[:at])
	domain := s[at:]
	if len(local) <= 2 {
		return string(local) + domain
	}
	return string(local[:2]) + strings.Repeat("*", len(local)-2) + domain
}

// applyOfficerMasking — 요청자 권한에 따라 담당자 정보를 마스킹한다.
//   - system_admin: 마스킹 없음(원본 그대로).
//   - 그 외: 마스킹. 이 공고에 파이프라인이 있으면(참여검토 시작) [공개]용 Full* 값도 함께 준다.
func applyOfficerMasking(d *noticeRawDetail, isAdmin, hasPipeline bool) {
	if d == nil {
		return
	}
	if isAdmin {
		d.OfficerMasked = false
		d.OfficerCanReveal = false
		return
	}
	fullName, fullPhone, fullEmail := d.OfficerName, d.OfficerPhone, d.OfficerEmail
	d.OfficerName = maskOfficerName(fullName)
	d.OfficerPhone = maskOfficerPhone(fullPhone)
	d.OfficerEmail = maskOfficerEmail(fullEmail)
	d.OfficerMasked = true
	if hasPipeline {
		d.OfficerCanReveal = true
		d.OfficerFullName = fullName
		d.OfficerFullPhone = fullPhone
		d.OfficerFullEmail = fullEmail
	}
}

// fetchNoticeRawDetail loads and parses the raw g2b JSON for a notice
// version. Returns (nil, nil) when there's nothing to show — no raw
// document, or content that doesn't parse as g2b's shape (e.g. the bundled
// demo source) — callers should treat that as "no detail available", not
// as an error.
func (s *Server) fetchNoticeRawDetail(ctx context.Context, versionID string) (*noticeRawDetail, error) {
	var rawContent string
	err := s.db.QueryRowContext(ctx, `
		SELECT rd.raw_content
		FROM notice_versions nv
		JOIN raw_documents rd ON rd.id = nv.raw_document_id
		WHERE nv.id = $1`, versionID,
	).Scan(&rawContent)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var f g2bRawFields
	if err := json.Unmarshal([]byte(rawContent), &f); err != nil {
		return nil, nil
	}

	return &noticeRawDetail{
		NoticeKind:              f.NtceKindNm,
		ContractMethod:          f.CntrctCnclsMthdNm,
		AwardMethod:             f.SucsfbidMthdNm,
		AwardMethodDetail:       f.SucsfbidMthdAppStd,
		AwardLowerLimitRate:     f.SucsfbidLwltRate,
		RebidAllowed:            ynToBool(f.RbidPermsnYn),
		QualificationDeadlineAt: parseG2BDateTime(f.BidQlfctRgstDt),
		ApplicationStartAt:      parseG2BDateTime(f.BidBeginDt),
		ApplicationEndAt:        parseG2BDateTime(f.BidClseDt),
		BidOpeningAt:            parseG2BDateTime(f.OpengDt),
		EstimatedPrice:          parseG2BAmount(f.PresmptPrce),
		Vat:                     parseG2BAmount(f.VAT),
		OfficerName:             f.NtceInsttOfclNm,
		OfficerPhone:            f.NtceInsttOfclTelNo,
		IndustryRestricted:      ynToBool(f.IndstrytyLmtYn),
		RegionRestrictionBasis:  f.RgnLmtBidLocplcJdgmBssNm,
		// Phase B 추가 매핑 — 시각은 parseG2BDateTime(KST 고정), 금액/개수는 parseG2BAmount.
		NoticeNo:               f.BidNtceNo,
		NoticeOrd:              f.BidNtceOrd,
		ReferenceNo:            f.RefNo,
		UnifiedNoticeNo:        f.UntyNtceNo,
		ServiceDivision:        f.SrvceDivNm,
		PublishedAt:            parseG2BDateTime(f.BidNtceDt),
		BidMethod:              f.BidMethdNm,
		InternationalBid:       ynToBool(f.IntrbidYn),
		PrearrangedPriceMethod: f.PrearngPrceDcsnMthdNm,
		PriceDrawCount:         parseG2BAmount(f.DrwtPrdprcNum),
		PriceTotalCount:        parseG2BAmount(f.TotPrdprcNum),
		JointContractMethod:    f.CmmnSpldmdMethdNm,
		OpeningPlace:           f.OpengPlce,
		RebidOpeningAt:         parseG2BDateTime(f.RbidOpengDt),
		AssignedBudget:         parseG2BAmount(f.AsignBdgtAmt),
		DemandInstitution:      f.DminsttNm,
		NoticeInstitution:      f.NtceInsttNm,
		OfficerEmail:           f.NtceInsttOfclEmailAdrs,
		BriefingAt:             parseG2BDateTime(f.DcmtgOprtnDt),
		BriefingPlace:          f.DcmtgOprtnPlce,
		ParticipationFee:       parseG2BAmount(f.BidPrtcptFee),
		BidGuaranteeRequired:   ynToBool(f.BidGrntymnyPaymntYn),
		// Phase B 확장 매핑.
		ReNotice:               ynToBool(f.ReNtceYn),
		PerformanceReview:      ynToBool(f.ArsltCmptYn),
		PerformanceDocMethod:   f.ArsltApplDocRcptMthdNm,
		ProductClassRestricted: ynToBool(f.PrdctClsfcLmtYn),
		JointRegionRestricted:  ynToBool(f.CmmnSpldmdCorpRgnLmtYn),
		JointAgreementMethod:   f.CmmnSpldmdAgrmntRcptdocMthd,
		Executive:              f.ExctvNm,
		OrderPlanNo:            f.OrderPlanUntyNo,
		PreSpecRegNo:           f.BfSpecRgstNo,
		Creditor:               f.CrdtrNm,
		ProcurementClass:       joinNonEmpty(" › ", f.PubPrcrmntLrgClsfcNm, f.PubPrcrmntMidClsfcNm, f.PubPrcrmntClsfcNm),
	}, nil
}

// joinNonEmpty — 비어있지 않은 조각만 sep로 연결(조달분류 대›중›세 등).
func joinNonEmpty(sep string, parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return strings.Join(out, sep)
}

func ynToBool(v string) *bool {
	switch v {
	case "Y":
		t := true
		return &t
	case "N":
		f := false
		return &f
	default:
		return nil
	}
}

// parseG2BDateTime tries both observed g2b timestamp layouts — most fields
// carry seconds ("2006-01-02 15:04:05"), but bidQlfctRgstDt has been
// observed without them ("2006-01-02 15:04"). 나라장터 일시는 타임존 표기 없는 KST 벽시계라
// 항상 KST로 해석한다(kstLocation() = Asia/Seoul, 실패 시 FixedZone +9 — distroless tzdata
// 없음 대비). 예전 time.Local 파싱은 운영(UTC)에서 +9시간 어긋났다. 이 함수는 raw_content를
// 요청마다 재파싱하므로, 여기만 고쳐도 기존 공고 상세 시각이 백필 없이 즉시 정상화된다.
func parseG2BDateTime(v string) *time.Time {
	if v == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, v, kstLocation()); err == nil {
			return &t
		}
	}
	return nil
}

func parseG2BAmount(v string) *int64 {
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

type attachmentItem struct {
	ID               string `json:"id"`
	OriginalFilename string `json:"originalFilename"`
	FileType         string `json:"fileType"`
	FileSizeBytes    *int64 `json:"fileSizeBytes"`
	DownloadURL      string `json:"downloadUrl"`
	DownloadStatus   string `json:"downloadStatus"`
	// Role — B-2. 지원사업 상세에서 "공고문"(SUPPORT_PRINT_DOCUMENT)과
	// "별첨자료"(SUPPORT_ATTACHMENT)를 구분해 보여주기 위함. g2b 첨부는 빈값.
	Role string `json:"role,omitempty"`
}

// supportDetailDTO — 지원사업 전용 공식 데이터(B-2). notice_type=support_program일
// 때만 채워진다. business_summary는 평문(text)만 노출한다(원본 HTML은 서버 보관 —
// 프론트 XSS 회피). applicationUrl은 원본 그대로 주고, href 안전검증은 프론트가 한다.
type supportDetailDTO struct {
	SupportTarget       string  `json:"supportTarget,omitempty"`
	BusinessSummaryText string  `json:"businessSummaryText,omitempty"`
	ApplicationMethod   string  `json:"applicationMethod,omitempty"`
	ReferenceContact    string  `json:"referenceContact,omitempty"`
	ApplicationURL      string  `json:"applicationUrl,omitempty"`
	CategoryMajor       string  `json:"categoryMajor,omitempty"`
	CategoryMiddle      string  `json:"categoryMiddle,omitempty"`
	Hashtags            string  `json:"hashtags,omitempty"`
	InquiryCount        *int64  `json:"inquiryCount,omitempty"`
	SourceUpdatedAt     *string `json:"sourceUpdatedAt,omitempty"`
}

// supportConditionsDTO — 지원사업 공고문에서 규칙 기반으로 뽑은 상세 신청조건(B-3).
// 공식 분류(supportDetailDTO)와 역할이 분리된다 — 이건 공고문 근거 상세조건이다.
// notice_type=support_program이고 규칙 추출이 끝난 공고에서만 채워진다.
type supportConditionsDTO struct {
	EligibilityText      string               `json:"eligibilityText,omitempty"`
	RequiredDocuments    []supportRequiredDoc `json:"requiredDocuments"`
	SupportAmountText    string               `json:"supportAmountText,omitempty"`
	SupportLimitText     string               `json:"supportLimitText,omitempty"`
	SupportLimitAmount   *int64               `json:"supportLimitAmount,omitempty"`
	SupportRateText      string               `json:"supportRateText,omitempty"`
	SupportScaleText     string               `json:"supportScaleText,omitempty"`
	BusinessAgeCondition string               `json:"businessAgeCondition,omitempty"`
	RevenueCondition     string               `json:"revenueCondition,omitempty"`
	RegionCondition      string               `json:"regionCondition,omitempty"`
	ExclusionConditions  []string             `json:"exclusionConditions"`
	PreferenceConditions []string             `json:"preferenceConditions"`
	SelectionProcess     string               `json:"selectionProcess,omitempty"`
	Confidence           string               `json:"confidence,omitempty"`
	NeedsAI              bool                 `json:"needsAi"`
	TextPoor             bool                 `json:"textPoor"`
	ExtractionMethod     string               `json:"extractionMethod,omitempty"`
}

// fetchSupportConditions returns the rule-extracted detailed conditions for a
// support-program notice, or nil when there's no row (procurement, or not yet
// extracted). B-3.
func (s *Server) fetchSupportConditions(ctx context.Context, noticeID string) *supportConditionsDTO {
	var d supportConditionsDTO
	var elig, amountT, limitT, rateT, scaleT, ageC, revC, regionC, sel, conf, method sql.NullString
	var limitAmt sql.NullInt64
	var reqDocs, excl, pref []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT eligibility_text, required_documents, support_amount_text, support_limit_text, support_limit_amount,
		       support_rate_text, support_scale_text, business_age_condition, revenue_condition, region_condition,
		       exclusion_conditions, preference_conditions, selection_process, confidence, needs_ai, text_poor, extraction_method
		FROM support_program_conditions WHERE notice_id = $1`, noticeID).
		Scan(&elig, &reqDocs, &amountT, &limitT, &limitAmt, &rateT, &scaleT, &ageC, &revC, &regionC,
			&excl, &pref, &sel, &conf, &d.NeedsAI, &d.TextPoor, &method)
	if err != nil {
		return nil // sql.ErrNoRows(입찰/미추출) 포함
	}
	d.EligibilityText = elig.String
	d.SupportAmountText = amountT.String
	d.SupportLimitText = limitT.String
	if limitAmt.Valid {
		d.SupportLimitAmount = &limitAmt.Int64
	}
	d.SupportRateText = rateT.String
	d.SupportScaleText = scaleT.String
	d.BusinessAgeCondition = ageC.String
	d.RevenueCondition = revC.String
	d.RegionCondition = regionC.String
	d.SelectionProcess = sel.String
	d.Confidence = conf.String
	d.ExtractionMethod = method.String
	d.RequiredDocuments = []supportRequiredDoc{}
	d.ExclusionConditions = []string{}
	d.PreferenceConditions = []string{}
	_ = json.Unmarshal(reqDocs, &d.RequiredDocuments)
	_ = json.Unmarshal(excl, &d.ExclusionConditions)
	_ = json.Unmarshal(pref, &d.PreferenceConditions)
	return &d
}

// fetchSupportProgramDetail returns the support-program official detail for a
// notice, or nil when there's no row (procurement notices, or not yet collected).
func (s *Server) fetchSupportProgramDetail(ctx context.Context, noticeID string) *supportDetailDTO {
	var d supportDetailDTO
	var target, text, method, contact, url, catMajor, catMid, tags sql.NullString
	var inquiry sql.NullInt64
	var updated sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT support_target, business_summary_text, application_method, reference_contact, application_url,
		       support_category_major, support_category_middle, hashtags, inquiry_count, source_updated_at
		FROM support_program_details WHERE notice_id = $1`, noticeID).
		Scan(&target, &text, &method, &contact, &url, &catMajor, &catMid, &tags, &inquiry, &updated)
	if err != nil {
		return nil // sql.ErrNoRows(미수집/입찰) 포함 — 없으면 nil
	}
	d.SupportTarget = target.String
	d.BusinessSummaryText = text.String
	d.ApplicationMethod = method.String
	d.ReferenceContact = contact.String
	d.ApplicationURL = url.String
	d.CategoryMajor = catMajor.String
	d.CategoryMiddle = catMid.String
	d.Hashtags = tags.String
	if inquiry.Valid {
		d.InquiryCount = &inquiry.Int64
	}
	if updated.Valid {
		v := updated.Time.Format("2006-01-02")
		d.SourceUpdatedAt = &v
	}
	return &d
}

// listAttachments surfaces the download_url g2b originally served the file
// from (verified to work via plain GET — see g2b Collector) rather than
// proxying through a new local-file-serving endpoint, since that's simpler
// and the attachments table already tracks whether the download completed.
func (s *Server) listAttachments(ctx context.Context, versionID string) ([]attachmentItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, original_filename, COALESCE(file_type, ''), file_size_bytes, COALESCE(download_url, ''), download_status,
		       COALESCE(attachment_role, '')
		FROM attachments
		WHERE notice_version_id = $1
		ORDER BY created_at`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []attachmentItem{}
	for rows.Next() {
		var it attachmentItem
		var size sql.NullInt64
		if err := rows.Scan(&it.ID, &it.OriginalFilename, &it.FileType, &size, &it.DownloadURL, &it.DownloadStatus, &it.Role); err != nil {
			continue
		}
		if size.Valid {
			it.FileSizeBytes = &size.Int64
		}
		out = append(out, it)
	}
	return out, nil
}
