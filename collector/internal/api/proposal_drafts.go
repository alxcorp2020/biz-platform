// proposal_drafts.go — 평가기준 맞춤 제안서 1차(2026-08-16): 작성 준비(readiness) →
// 초안 생성 → 미리보기/수정 → 수정 가능한 DOCX 다운로드.
//
// API(기존 스타일: /api/notices/{id}/… child + 리소스 단위):
//
//	GET  /api/notices/{id}/proposal-readiness   평가기준·회사정보 대응·질문·기존 초안 (유료)
//	POST /api/notices/{id}/proposal-drafts      초안 생성 {answers, force} (유료; 같은 회사×공고×버전 초안 있으면 409 draft_exists)
//	GET  /api/proposal-drafts/{id}              초안 조회(+stale 여부) (유료·소유 회사만)
//	PATCH /api/proposal-drafts/{id}             섹션 본문/제목 수정 (유료·소유 회사만)
//	GET  /api/proposal-drafts/{id}/docx         DOCX 생성·다운로드 (유료·소유 회사만)
//
// 원칙:
//   - 유료 게이트는 requirePaidFeature(entitlements.go)로 모든 엔드포인트에서 서버 검사.
//   - 초안 본문은 결정론적 composer가 만든다: 공고 사실 + 평가기준(배점 순서 그대로) +
//     회사 canonical 사실 + 사용자 답변만 사용. 회사 DB/답변에 없는 사실은 절대 쓰지 않고
//     "[확인 필요: …]"로 남긴다(DOCX에도 그대로). 생성 모델에 자유작성시키지 않는다.
//   - 초안은 생성 당시 평가기준/회사정보/답변 스냅샷을 가진다. 공고 버전이 바뀌면
//     stale=true로 경고만 하고 몰래 바꾸지 않는다(새 초안은 사용자 명시 선택).
//   - 사용자 화면 문구에 "AI"를 쓰지 않는다.
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"biz-platform/collector/internal/billing"
	"biz-platform/collector/internal/docx"
)

const (
	proposalDraftStatusDraft = "draft"
	proposalDisclaimer       = "본 문서는 공고자료와 등록된 회사정보를 기준으로 구성된 초안입니다. 제출 전 공고 원문 및 실제 회사정보와의 일치 여부를 반드시 확인하세요."
	proposalDraftEngine      = "structured-composer-v1"
	confirmNeededPrefix      = "[확인 필요: "
)

// ---- 공고 사실 ----

type proposalNoticeFacts struct {
	NoticeID         string   `json:"noticeId"`
	NoticeVersionID  string   `json:"noticeVersionId"`
	VersionNumber    int      `json:"versionNumber"`
	NoticeType       string   `json:"noticeType"`
	Title            string   `json:"title"`
	OrganizationName string   `json:"organizationName"`
	ExternalNoticeID string   `json:"externalNoticeId,omitempty"`
	BudgetAmount     *int64   `json:"budgetAmount,omitempty"`
	ApplicationEndAt string   `json:"applicationEndAt,omitempty"`
	OpeningAt        string   `json:"openingAt,omitempty"`
	AwardMethod      string   `json:"awardMethod,omitempty"`
	OfficialURL      string   `json:"officialUrl,omitempty"`
	SummaryLines     []string `json:"summaryLines"`
}

func (s *Server) loadProposalNoticeFacts(ctx context.Context, noticeID string) (*proposalNoticeFacts, error) {
	f := &proposalNoticeFacts{NoticeID: noticeID, SummaryLines: []string{}}
	var ext, org, method, official sql.NullString
	var budget sql.NullInt64
	var appEnd, opening sql.NullTime
	var currentVersion int
	err := s.db.QueryRowContext(ctx, `SELECT notice_type, title, organization_name, external_notice_id, budget_amount,
			COALESCE(application_end_datetime, application_end_at::timestamptz), opening_at, success_bid_method_name, official_url, current_version
		FROM notices WHERE id = $1`, noticeID).Scan(&f.NoticeType, &f.Title, &org, &ext, &budget, &appEnd, &opening, &method, &official, &currentVersion)
	if err != nil {
		return nil, err
	}
	f.OrganizationName, f.ExternalNoticeID, f.AwardMethod, f.OfficialURL = org.String, ext.String, method.String, official.String
	if budget.Valid {
		f.BudgetAmount = &budget.Int64
	}
	if appEnd.Valid {
		f.ApplicationEndAt = appEnd.Time.In(kst()).Format("2006-01-02 15:04")
	}
	if opening.Valid {
		f.OpeningAt = opening.Time.In(kst()).Format("2006-01-02 15:04")
	}
	f.VersionNumber = currentVersion
	vid, err := s.currentVersionID(ctx, noticeID, currentVersion)
	if err != nil {
		return nil, err
	}
	f.NoticeVersionID = vid
	if sum, err := s.fetchNoticeAISummary(ctx, vid); err == nil && sum != nil {
		f.SummaryLines = sum.Lines
	}
	return f, nil
}

func kst() *time.Location {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		return time.FixedZone("KST", 9*3600)
	}
	return loc
}

// ---- 초안 내용 모델(JSONB content) ----

type draftTable struct {
	Title  string     `json:"title,omitempty"`
	Header []string   `json:"header"`
	Rows   [][]string `json:"rows"`
}

type draftSection struct {
	ID          string       `json:"id"`
	CriterionID string       `json:"criterionId,omitempty"`
	Title       string       `json:"title"`
	Score       *float64     `json:"score,omitempty"`
	Kind        string       `json:"kind"`
	Body        string       `json:"body"`
	Guidance    []string     `json:"guidance"`
	Bullets     []string     `json:"bullets"`
	Tables      []draftTable `json:"tables"`
	Missing     []string     `json:"missing"`
}

type draftContent struct {
	Title        string                `json:"title"`
	Engine       string                `json:"engine"`
	Sections     []draftSection        `json:"sections"`
	Requirements []proposalRequirement `json:"requirements"`
	Missing      []string              `json:"missing"`
	Disclaimer   string                `json:"disclaimer"`
}

// answerValue — 질문 답변(초안에만 저장).
type answerValue struct {
	Value string `json:"value,omitempty"`
	Text  string `json:"text,omitempty"`
}

func won(v int64) string { return formatKRWAmount(v) + "원" }

func confirm(msg string) string { return confirmNeededPrefix + msg + "]" }

// composeProposalDraft — 결정론적 초안 구성. 사실 출처는 notice/set/facts/answers뿐.
func composeProposalDraft(notice *proposalNoticeFacts, set *evaluationCriteriaSet, facts *companyFacts, answers map[string]answerValue) draftContent {
	content := draftContent{Title: notice.Title + " 제안서(초안)", Engine: proposalDraftEngine, Requirements: set.Requirements, Disclaimer: proposalDisclaimer}
	if content.Requirements == nil {
		content.Requirements = []proposalRequirement{}
	}
	var allMissing []string
	addMissing := func(sec *draftSection, msg string) {
		m := confirm(msg)
		sec.Missing = append(sec.Missing, m)
		allMissing = append(allMissing, sec.Title+" — "+m)
	}
	companyName := facts.CompanyName
	if companyName == "" {
		companyName = "[확인 필요: 회사명]"
	}

	// 0. 제안사 개요(회사 canonical만).
	intro := draftSection{ID: "s0", Title: "제안사 개요", Kind: "company", Guidance: []string{}, Bullets: []string{}, Tables: []draftTable{}, Missing: []string{}}
	introRows := [][]string{}
	// 개요의 [확인 필요]는 표 셀에 그대로 두고(중복 목록 없이) 부록 목록에만 모은다.
	kv := func(k, v, missingMsg string) {
		if strings.TrimSpace(v) == "" {
			v = confirm(missingMsg)
			allMissing = append(allMissing, intro.Title+" — "+v)
		}
		introRows = append(introRows, []string{k, v})
	}
	kv("회사명", facts.CompanyName, "회사명을 입력해 주세요.")
	kv("대표자", facts.RepresentativeName, "대표자명을 입력해 주세요.")
	kv("소재지", facts.Address, "회사 주소를 입력해 주세요.")
	if facts.FoundingDate != "" {
		age := ""
		if facts.BusinessAgeYears != nil {
			age = fmt.Sprintf(" (업력 %.1f년)", *facts.BusinessAgeYears)
		}
		introRows = append(introRows, []string{"설립일", facts.FoundingDate + age})
	} else {
		kv("설립일", "", "설립일(개업일)을 입력해 주세요.")
	}
	if facts.EmployeeCount != nil {
		introRows = append(introRows, []string{"직원수", fmt.Sprintf("%d명", *facts.EmployeeCount)})
	} else {
		kv("직원수", "", "직원수를 입력해 주세요.")
	}
	if facts.RevenueAmount != nil {
		introRows = append(introRows, []string{"매출액", won(*facts.RevenueAmount)})
	} else {
		kv("매출액", "", "최근 결산 매출액을 입력해 주세요.")
	}
	if len(facts.Industry) > 0 {
		introRows = append(introRows, []string{"업종", strings.Join(facts.Industry, ", ")})
	}
	if len(facts.BusinessType) > 0 {
		introRows = append(introRows, []string{"업태", strings.Join(facts.BusinessType, ", ")})
	}
	intro.Tables = append(intro.Tables, draftTable{Title: "회사 개요", Header: []string{"항목", "내용"}, Rows: introRows})
	intro.Body = fmt.Sprintf("%s는(은) %s이(가) 공고한 「%s」 사업에 다음과 같이 제안합니다.", companyName, orDash(notice.OrganizationName), notice.Title)
	content.Sections = append(content.Sections, intro)

	// 1..N. 평가항목 순서 그대로(배점 높은 항목을 축약하지 않는다).
	for i, c := range set.Criteria {
		kind := classifyCriterionKind(c)
		sec := draftSection{ID: "s" + strconv.Itoa(i+1), CriterionID: c.ID, Title: c.Title, Score: c.Score, Kind: kind, Guidance: []string{}, Bullets: []string{}, Tables: []draftTable{}, Missing: []string{}}
		if c.Description != "" {
			sec.Guidance = append(sec.Guidance, "평가내용: "+c.Description)
		}
		for _, sc := range c.SubCriteria {
			sec.Guidance = append(sec.Guidance, "세부기준: "+sc)
		}
		ans, hasAns := answers["q_"+c.ID]
		ansText := strings.TrimSpace(ans.Text)
		switch kind {
		case kindUnderstanding:
			sec.Body = fmt.Sprintf("본 사업은 %s이(가) 발주하는 「%s」입니다.", orDash(notice.OrganizationName), notice.Title)
			sec.Bullets = append(sec.Bullets, noticeFactBullets(notice)...)
			if len(notice.SummaryLines) > 0 {
				sec.Bullets = append(sec.Bullets, "공고자료 요약:")
				for _, l := range notice.SummaryLines {
					sec.Bullets = append(sec.Bullets, "  - "+l)
				}
			}
			addMissing(&sec, "사업 배경·목적에 대한 당사의 이해와 접근 방향을 서술해 주세요.")
		case kindPlan:
			sec.Body = fmt.Sprintf("「%s」의 요구사항을 충족하기 위한 수행 방안을 아래 평가기준 항목별로 기술합니다.", notice.Title)
			if len(c.SubCriteria) > 0 {
				for _, sc := range c.SubCriteria {
					sec.Bullets = append(sec.Bullets, sc+": "+confirm("이 세부기준에 대한 당사의 수행 방안을 작성해 주세요."))
				}
			} else {
				addMissing(&sec, "이 항목의 수행 방안(추진 전략·방법·일정 등)을 작성해 주세요.")
			}
			if len(c.SubCriteria) > 0 {
				allMissing = append(allMissing, sec.Title+" — 세부기준별 수행 방안 작성 필요")
			}
		case kindTrackRecord:
			if len(facts.TrackRecords) > 0 {
				sec.Body = fmt.Sprintf("당사는 다음과 같이 등록된 수행실적 %d건을 보유하고 있습니다.", len(facts.TrackRecords))
				rows := [][]string{}
				for _, t := range facts.TrackRecords {
					period := strings.TrimSpace(t.PeriodStart + " ~ " + t.PeriodEnd)
					if period == "~" {
						period = "-"
					}
					amt := "-"
					if t.ContractAmount != nil {
						amt = won(*t.ContractAmount)
					}
					done := "-"
					if t.IsCompleted != nil {
						if *t.IsCompleted {
							done = "완료"
						} else {
							done = "진행중"
						}
					}
					rows = append(rows, []string{t.ProjectName, orDash(t.ClientName), period, amt, done})
				}
				sec.Tables = append(sec.Tables, draftTable{Title: "수행실적", Header: []string{"사업명", "발주처", "수행기간", "계약금액", "상태"}, Rows: rows})
				addMissing(&sec, "위 실적 중 본 사업과 유사한 실적을 선별하고, 필요 시 증빙(계약서·실적증명서)을 첨부해 주세요.")
			} else {
				sec.Body = "등록된 수행실적이 없어 실적 내용을 자동으로 구성하지 않았습니다."
				addMissing(&sec, "본 사업과 유사한 수행실적(사업명·발주처·기간·금액)을 등록하거나 직접 기재해 주세요.")
			}
		case kindPersonnel:
			if len(facts.Personnel) > 0 {
				sec.Body = fmt.Sprintf("당사는 등록된 인력 %d명으로 수행조직을 구성합니다.", len(facts.Personnel))
				rows := [][]string{}
				for _, p := range facts.Personnel {
					yrs := "-"
					if p.CareerYears != nil {
						yrs = fmt.Sprintf("%.0f년", *p.CareerYears)
					}
					rows = append(rows, []string{orDash(p.Role), orDash(p.TechField), yrs, orDash(p.TechGrade), orDash(strings.Join(p.Qualifications, ", ")), orDash(p.RecentProject)})
				}
				sec.Tables = append(sec.Tables, draftTable{Title: "투입 인력", Header: []string{"역할", "기술분야", "경력", "등급", "자격", "최근 수행사업"}, Rows: rows})
			} else {
				sec.Body = "등록된 인력 정보가 없어 수행조직을 자동으로 구성하지 않았습니다."
			}
			pmDone := false
			if hasAns {
				if strings.HasPrefix(ans.Value, "personnel:") {
					if idx, err := strconv.Atoi(strings.TrimPrefix(ans.Value, "personnel:")); err == nil && idx >= 0 && idx < len(facts.Personnel) {
						p := facts.Personnel[idx]
						sec.Bullets = append(sec.Bullets, "사업 책임자(PM): "+orDash(p.Role)+" / "+orDash(p.TechField))
						pmDone = true
					}
				}
				if ansText != "" {
					sec.Bullets = append(sec.Bullets, "사업 책임자(PM) — 사용자 입력: "+ansText)
					pmDone = true
				}
			}
			if !pmDone {
				addMissing(&sec, "본 사업에 투입할 책임자(PM)와 관련 경력을 입력해 주세요.")
			}
			addMissing(&sec, "인력별 역할·투입 기간(투입률)을 확인해 주세요.")
		case kindCredential:
			if len(facts.Credentials) > 0 {
				sec.Body = fmt.Sprintf("당사가 보유한 면허·인증·지식재산 %d건은 다음과 같습니다.", len(facts.Credentials))
				rows := [][]string{}
				for _, cr := range facts.Credentials {
					kindLabel := map[string]string{"license": "면허", "certification": "인증", "ip": "지식재산"}[cr.Kind]
					rows = append(rows, []string{kindLabel, cr.Name, orDash(cr.RegistrationNumber), orDash(cr.IssuingAuthority), orDash(cr.IssuedAt), orDash(cr.ExpiresAt)})
				}
				sec.Tables = append(sec.Tables, draftTable{Title: "보유 면허·인증·지식재산", Header: []string{"구분", "명칭", "등록번호", "발급기관", "취득일", "만료일"}, Rows: rows})
			} else {
				sec.Body = "등록된 면허·인증·지식재산이 없습니다."
			}
			if hasAns && ansText != "" {
				sec.Bullets = append(sec.Bullets, "사용자 입력: "+ansText)
			} else if !hasAns || ans.Value != "none" {
				addMissing(&sec, "이 항목에 반영할 인증·특허·자격과 증빙을 확인해 주세요.")
			}
		case kindFinancial:
			if len(facts.Financials) > 0 {
				rows := [][]string{}
				for _, fin := range facts.Financials {
					r := []string{strconv.Itoa(fin.FiscalYear), "-", "-", "-", orDash(fin.CreditRating)}
					if fin.Revenue != nil {
						r[1] = won(*fin.Revenue)
					}
					if fin.OperatingProfit != nil {
						r[2] = won(*fin.OperatingProfit)
					}
					if fin.DebtRatio != nil {
						r[3] = *fin.DebtRatio + "%"
					}
					rows = append(rows, r)
				}
				sec.Body = "당사의 등록된 재무현황은 다음과 같습니다."
				sec.Tables = append(sec.Tables, draftTable{Title: "재무현황", Header: []string{"회계연도", "매출액", "영업이익", "부채비율", "신용등급"}, Rows: rows})
			} else if facts.CreditRating != "" {
				sec.Body = "당사의 신용평가등급: " + facts.CreditRating
			} else {
				sec.Body = "등록된 재무·신용 정보가 없습니다."
			}
			if hasAns && ansText != "" {
				sec.Bullets = append(sec.Bullets, "사용자 입력: "+ansText)
			} else if len(facts.Financials) == 0 && facts.CreditRating == "" {
				addMissing(&sec, "신용평가등급·재무제표 등 정량평가 증빙을 확인해 주세요.")
			}
		case kindManagement:
			sec.Body = fmt.Sprintf("「%s」에 대한 운영·안전·사후관리 지원 체계는 다음과 같습니다.", c.Title)
			if hasAns {
				switch ans.Value {
				case "business_hours":
					sec.Bullets = append(sec.Bullets, "대응 시간: 평일 업무시간")
				case "24h":
					sec.Bullets = append(sec.Bullets, "대응 시간: 24시간")
				}
				if ansText != "" {
					sec.Bullets = append(sec.Bullets, "사용자 입력: "+ansText)
				}
			}
			if len(sec.Bullets) == 0 {
				addMissing(&sec, "대응 시간·담당 조직·하자보수(비상대응) 절차를 입력해 주세요.")
			} else {
				addMissing(&sec, "하자보수 기간·담당 조직·비상대응 절차 등 세부 조건을 확인해 주세요.")
			}
		case kindPrice:
			sec.Body = "가격 제안은 입찰서(가격 제안서)로 별도 제출합니다."
			addMissing(&sec, "공고의 가격평가 산식과 제출 방식을 확인해 주세요.")
		default:
			sec.Body = fmt.Sprintf("「%s」 항목은 공고 평가기준에 따라 아래 내용을 기술합니다.", c.Title)
			if hasAns && ansText != "" {
				sec.Bullets = append(sec.Bullets, "사용자 입력: "+ansText)
			} else {
				addMissing(&sec, "이 항목에 반영할 회사 내용을 작성해 주세요.")
			}
		}
		content.Sections = append(content.Sections, sec)
	}
	if allMissing == nil {
		allMissing = []string{}
	}
	content.Missing = allMissing
	return content
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func noticeFactBullets(n *proposalNoticeFacts) []string {
	var out []string
	if n.OrganizationName != "" {
		out = append(out, "발주기관: "+n.OrganizationName)
	}
	if n.ExternalNoticeID != "" {
		out = append(out, "공고번호: "+n.ExternalNoticeID)
	}
	if n.BudgetAmount != nil {
		out = append(out, "사업예산(기초금액): "+won(*n.BudgetAmount))
	}
	if n.AwardMethod != "" {
		out = append(out, "낙찰방식: "+n.AwardMethod)
	}
	if n.ApplicationEndAt != "" {
		out = append(out, "입찰(제안서) 마감: "+n.ApplicationEndAt)
	}
	return out
}

// ---- DOCX ----

func buildProposalDocx(content draftContent, notice *proposalNoticeFacts, facts *companyFacts, createdAt time.Time) ([]byte, error) {
	d := docx.New()
	d.CoreTitle = content.Title
	d.CoreCreator = orDash(facts.CompanyName)
	d.FooterNote = "제출 전 공고 원문·회사정보와 일치 여부를 확인하세요"
	// 표지 — 문서 제목(사용자가 수정했으면 그 값), 공고명, 발주기관, 제안사, 작성일.
	title := strings.TrimSpace(content.Title)
	if title == "" {
		title = notice.Title + " 제안서(초안)"
	}
	d.Title(title)
	d.Centered("")
	d.Centered("공고명: " + notice.Title)
	d.Centered("발주기관: " + orDash(notice.OrganizationName))
	d.Centered("제안사: " + orDash(facts.CompanyName))
	d.Centered("작성일: " + createdAt.In(kst()).Format("2006년 01월 02일"))
	d.Centered("")
	d.Note(content.Disclaimer)
	d.PageBreak()
	// 목차
	d.Heading(1, "목차")
	d.TOCField("Word에서 목차 필드를 갱신하면(F9) 제목이 자동으로 채워집니다.")
	for i, sec := range content.Sections {
		label := sec.Title
		if i > 0 {
			label = fmt.Sprintf("%d. %s", i, sec.Title)
		}
		if sec.Score != nil {
			label += fmt.Sprintf(" (%s점)", trimFloat(*sec.Score))
		}
		d.Paragraph(label)
	}
	if len(content.Requirements) > 0 {
		d.Heading(2, "공고의 제안서 작성 요구사항")
		rows := [][]string{}
		for _, r := range content.Requirements {
			rows = append(rows, []string{r.Label, r.Value})
		}
		d.Table([]string{"항목", "요구사항"}, rows)
	}
	d.PageBreak()
	// 본문
	for i, sec := range content.Sections {
		title := sec.Title
		if i > 0 {
			title = fmt.Sprintf("%d. %s", i, sec.Title)
		}
		if sec.Score != nil {
			title += fmt.Sprintf(" (%s점)", trimFloat(*sec.Score))
		}
		d.Heading(1, title)
		if len(sec.Guidance) > 0 {
			d.Note("작성 안내(공고 평가기준): " + strings.Join(sec.Guidance, " / "))
		}
		if strings.TrimSpace(sec.Body) != "" {
			d.Paragraph(sec.Body)
		}
		if len(sec.Bullets) > 0 {
			d.Bullets(sec.Bullets)
		}
		for _, t := range sec.Tables {
			if t.Title != "" {
				d.Heading(3, t.Title)
			}
			d.Table(t.Header, t.Rows)
		}
		if len(sec.Missing) > 0 {
			d.Bullets(sec.Missing)
		}
	}
	// 부록
	d.PageBreak()
	d.Heading(1, "부록. 확인 필요 항목")
	if len(content.Missing) == 0 {
		d.Paragraph("확인 필요 항목이 없습니다.")
	} else {
		d.Bullets(content.Missing)
	}
	d.Note(content.Disclaimer)
	return d.Bytes()
}

func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

var unsafeFilenameRe = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]+`)

// proposalDocxFilename — 공고명_제안서초안_회사명_YYYYMMDD.docx (unsafe 문자 제거, 길이 제한).
func proposalDocxFilename(noticeTitle, companyName string, at time.Time) string {
	clean := func(s string, max int) string {
		s = unsafeFilenameRe.ReplaceAllString(s, " ")
		s = strings.Join(strings.Fields(s), " ")
		s = strings.ReplaceAll(s, " ", "_")
		r := []rune(s)
		if len(r) > max {
			r = r[:max]
		}
		return strings.Trim(string(r), "._")
	}
	nt := clean(noticeTitle, 40)
	if nt == "" {
		nt = "공고"
	}
	cn := clean(companyName, 20)
	if cn == "" {
		cn = "회사"
	}
	return fmt.Sprintf("%s_제안서초안_%s_%s.docx", nt, cn, at.In(kst()).Format("20060102"))
}

// ---- 저장/조회 ----

type proposalDraftRow struct {
	ID               string
	NoticeID         string
	NoticeVersionID  string
	CompanyProfileID string
	CreatedByUserID  sql.NullString
	Status           string
	Title            string
	Evaluation       *evaluationCriteriaSet
	Company          *companyFacts
	Answers          map[string]answerValue
	Content          draftContent
	Missing          []string
	GeneratedAt      sql.NullTime
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (s *Server) loadProposalDraft(ctx context.Context, id string) (*proposalDraftRow, error) {
	var d proposalDraftRow
	var evalRaw, compRaw, ansRaw, contRaw, missRaw []byte
	err := s.db.QueryRowContext(ctx, `SELECT id, notice_id, notice_version_id, company_profile_id, created_by_user_id, status, title,
			evaluation_snapshot, company_snapshot, answers, content, missing_information, generated_at, created_at, updated_at
		FROM proposal_drafts WHERE id = $1`, id).Scan(&d.ID, &d.NoticeID, &d.NoticeVersionID, &d.CompanyProfileID, &d.CreatedByUserID, &d.Status, &d.Title,
		&evalRaw, &compRaw, &ansRaw, &contRaw, &missRaw, &d.GeneratedAt, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(evalRaw) > 0 {
		d.Evaluation = &evaluationCriteriaSet{}
		_ = json.Unmarshal(evalRaw, d.Evaluation)
	}
	if len(compRaw) > 0 {
		d.Company = &companyFacts{}
		_ = json.Unmarshal(compRaw, d.Company)
	}
	d.Answers = map[string]answerValue{}
	if len(ansRaw) > 0 {
		_ = json.Unmarshal(ansRaw, &d.Answers)
	}
	if len(contRaw) > 0 {
		_ = json.Unmarshal(contRaw, &d.Content)
	}
	d.Missing = []string{}
	if len(missRaw) > 0 {
		_ = json.Unmarshal(missRaw, &d.Missing)
	}
	return &d, nil
}

// draftDTO — 응답 형태. stale: 공고 현재 버전이 초안 생성 당시 버전과 다름.
func (s *Server) draftDTO(ctx context.Context, d *proposalDraftRow) map[string]any {
	var currentVersionID string
	var noticeTitle string
	var currentVersion int
	_ = s.db.QueryRowContext(ctx, `SELECT title, current_version FROM notices WHERE id = $1`, d.NoticeID).Scan(&noticeTitle, &currentVersion)
	if v, err := s.currentVersionID(ctx, d.NoticeID, currentVersion); err == nil {
		currentVersionID = v
	}
	var draftVersionNumber int
	_ = s.db.QueryRowContext(ctx, `SELECT version_number FROM notice_versions WHERE id = $1`, d.NoticeVersionID).Scan(&draftVersionNumber)
	stale := currentVersionID != "" && currentVersionID != d.NoticeVersionID
	companyName := ""
	if d.Company != nil {
		companyName = d.Company.CompanyName
	}
	var generatedAt *time.Time
	if d.GeneratedAt.Valid {
		t := d.GeneratedAt.Time
		generatedAt = &t
	}
	return map[string]any{
		"id":                   d.ID,
		"noticeId":             d.NoticeID,
		"noticeTitle":          noticeTitle,
		"noticeVersionId":      d.NoticeVersionID,
		"noticeVersionNumber":  draftVersionNumber,
		"currentVersionNumber": currentVersion,
		"stale":                stale,
		"status":               d.Status,
		"title":                d.Title,
		"companyName":          companyName,
		"content":              d.Content,
		"missing":              d.Missing,
		"answers":              d.Answers,
		"evaluationSummary":    evaluationSummaryOf(d.Evaluation),
		"generatedAt":          generatedAt,
		"createdAt":            d.CreatedAt,
		"updatedAt":            d.UpdatedAt,
		"docxUrl":              "/api/proposal-drafts/" + d.ID + "/docx",
	}
}

func evaluationSummaryOf(set *evaluationCriteriaSet) map[string]any {
	if set == nil {
		return nil
	}
	return map[string]any{"criteriaCount": len(set.Criteria), "totalScore": set.TotalScore, "method": set.Method, "sourceDocuments": set.SourceDocs}
}

// existingDraftsFor — 회사×공고의 초안 목록(최신순, 요약).
func (s *Server) existingDraftsFor(ctx context.Context, profileID, noticeID, currentVersionID string) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id, d.notice_version_id, nv.version_number, d.title, d.created_at, d.updated_at
		FROM proposal_drafts d JOIN notice_versions nv ON nv.id = d.notice_version_id
		WHERE d.company_profile_id = $1 AND d.notice_id = $2 ORDER BY d.created_at DESC LIMIT 10`, profileID, noticeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, vid, title string
		var vn int
		var created, updated time.Time
		if err := rows.Scan(&id, &vid, &vn, &title, &created, &updated); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "noticeVersionId": vid, "noticeVersionNumber": vn, "title": title, "createdAt": created, "updatedAt": updated, "stale": vid != currentVersionID})
	}
	return out, rows.Err()
}

// ---- 핸들러 ----

// GET /api/notices/{id}/proposal-readiness
func (s *Server) handleProposalReadiness(w http.ResponseWriter, r *http.Request) {
	_, profile, ok := s.requirePaidFeature(w, r, billing.FeatureProposalDraftDocx, "proposal-readiness")
	if !ok {
		return
	}
	ctx := r.Context()
	noticeID := r.PathValue("id")
	notice, err := s.loadProposalNoticeFacts(ctx, noticeID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("proposal-readiness: notice lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	set, status, err := s.getOrExtractEvaluationCriteria(ctx, notice.NoticeVersionID, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		s.logger.Error("proposal-readiness: criteria failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	drafts, err := s.existingDraftsFor(ctx, profile.ID, noticeID, notice.NoticeVersionID)
	if err != nil {
		s.logger.Error("proposal-readiness: drafts lookup failed", "error", err)
		drafts = []map[string]any{}
	}
	if set == nil || status != evalStatusFound || len(set.Criteria) == 0 {
		// criteriaStatus=pending: 첨부 텍스트 추출(워커)이 아직 진행 중이라 판단을 보류한 상태 —
		// "찾지 못함"이 아니라 "아직 분석 중"이며 부정 캐시 없이 다음 확인에서 정상 추출된다.
		// 최상위 status는 기존 계약(ready | no_criteria)을 유지하고 문구/criteriaStatus로만 구분한다.
		message := "현재 수집된 공고자료에서 제안서 평가기준을 찾지 못했습니다. 공고 첨부파일의 제안요청서 또는 평가표를 확인해 주세요."
		if status == evalStatusPending {
			message = "첨부파일을 분석하고 있습니다. 잠시 후 다시 확인해 주세요."
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "no_criteria", "criteriaStatus": status, "notice": notice, "drafts": drafts,
			"message": message,
		})
		return
	}
	facts, err := s.loadCompanyFacts(ctx, profile.ID)
	if err != nil {
		s.logger.Error("proposal-readiness: company facts failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	items, questions, summary := buildReadiness(set, facts)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ready",
		"notice":       notice,
		"criteria":     set.Criteria,
		"totalScore":   set.TotalScore,
		"notes":        set.Notes,
		"requirements": set.Requirements,
		"method":       set.Method,
		"sourceDocs":   set.SourceDocs,
		"items":        items,
		"questions":    questions,
		"summary":      summary,
		"drafts":       drafts,
	})
}

// POST /api/notices/{id}/proposal-drafts  {answers:{qid:{value,text}}, force:bool}
func (s *Server) handleCreateProposalDraft(w http.ResponseWriter, r *http.Request) {
	userID, profile, ok := s.requirePaidFeature(w, r, billing.FeatureProposalDraftDocx, "proposal-draft-create")
	if !ok {
		return
	}
	ctx := r.Context()
	noticeID := r.PathValue("id")
	var body struct {
		Answers map[string]answerValue `json:"answers"`
		Force   bool                   `json:"force"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if body.Answers == nil {
		body.Answers = map[string]answerValue{}
	}
	s.logger.Info("proposal draft requested", "noticeId", noticeID, "profileId", profile.ID, "userId", userID, "force", body.Force)
	notice, err := s.loadProposalNoticeFacts(ctx, noticeID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("proposal-draft-create: notice lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	// 재생성 비용 방지: 같은 회사×공고×현재 버전 초안이 있으면 명시 force 없이는 새로 만들지 않는다.
	if !body.Force {
		var existingID string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM proposal_drafts WHERE company_profile_id = $1 AND notice_id = $2 AND notice_version_id = $3 ORDER BY created_at DESC LIMIT 1`, profile.ID, noticeID, notice.NoticeVersionID).Scan(&existingID)
		if err == nil && existingID != "" {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "draft_exists", "draftId": existingID})
			return
		}
	}
	set, status, err := s.getOrExtractEvaluationCriteria(ctx, notice.NoticeVersionID, false)
	if err != nil {
		s.logger.Error("proposal-draft-create: criteria failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if set == nil || status != evalStatusFound || len(set.Criteria) == 0 {
		// 근거 없이 일반 제안서를 만들지 않는다.
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no_evaluation_criteria", "criteriaStatus": status})
		return
	}
	facts, err := s.loadCompanyFacts(ctx, profile.ID)
	if err != nil {
		s.logger.Error("proposal-draft-create: company facts failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	content := composeProposalDraft(notice, set, facts, body.Answers)
	evalJSON, _ := json.Marshal(set)
	compJSON, _ := json.Marshal(facts)
	ansJSON, _ := json.Marshal(body.Answers)
	contJSON, _ := json.Marshal(content)
	missJSON, _ := json.Marshal(content.Missing)
	// 사용량(2026-08-18): 초안 INSERT와 사용량 소비를 한 트랜잭션으로 — composer는 결정론(외부 호출
	// 없음)이라 트랜잭션이 짧고, 한도 초과면 초안도 함께 롤백된다(실패 미차감). Free는 평생 체험
	// 1회(lifetime), 유료는 월 한도. subject=새 초안 id라 새 초안이 성공적으로 생성될 때만 1건.
	// 같은 공고 재생성(force)도 새 초안이므로 1건 — 기존 초안의 GET/PATCH/DOCX는 소비 없음.
	plan, err := s.effectivePlan(ctx, profile.ID)
	if err != nil {
		s.logger.Error("proposal-draft-create: plan lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	periodKey, limit := proposalDraftUsagePeriod(plan, s.effectivePlanInfo(ctx, plan), time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO proposal_drafts (notice_id, notice_version_id, company_profile_id, created_by_user_id, status, title,
			evaluation_snapshot, company_snapshot, answers, content, missing_information, generation_model, generated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9::jsonb,$10::jsonb,$11::jsonb,$12, now()) RETURNING id`,
		noticeID, notice.NoticeVersionID, profile.ID, userID, proposalDraftStatusDraft, content.Title,
		string(evalJSON), string(compJSON), string(ansJSON), string(contJSON), string(missJSON), proposalDraftEngine).Scan(&id)
	if err != nil {
		s.logger.Error("proposal draft generation failed", "error", err, "noticeId", noticeID, "profileId", profile.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	dec, err := s.consumeFeatureUsage(ctx, tx, profile.ID, billing.UsageProposalDraft, periodKey, id, limit)
	if err != nil {
		s.logger.Error("proposal-draft-create: usage consume failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if !dec.Allowed {
		// 롤백(defer) — 초안도 사용량도 남지 않는다.
		s.logger.Info("proposal draft quota exceeded", "plan", string(plan), "profileId", profile.ID, "used", dec.Used, "limit", dec.Limit)
		writeQuotaExceeded(w, string(billing.UsageProposalDraft), dec.Used, dec.Limit)
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	s.logger.Info("proposal draft generated", "draftId", id, "noticeId", noticeID, "profileId", profile.ID, "sections", len(content.Sections), "missing", len(content.Missing))
	d, err := s.loadProposalDraft(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusCreated, s.draftDTO(ctx, d))
}

// requireOwnedDraft — 인증 + 회사 + 소유(회사) 검사. 다른 회사 초안은 404(존재 여부도 숨김).
// 2026-08-18: 유료 게이트를 여기서 빼고 소유 검사만 한다 — Free 체험으로 만든 초안이나 플랜 강등
// 후의 기존 초안도 조회/수정/DOCX는 계속 가능해야 한다(새 초안 생성만 플랜/사용량 게이트).
func (s *Server) requireOwnedDraft(w http.ResponseWriter, r *http.Request, logTag string) (*proposalDraftRow, *companyProfileDTO, bool) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil, nil, false
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error(logTag+": profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return nil, nil, false
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return nil, nil, false
	}
	id := r.PathValue("id")
	d, err := s.loadProposalDraft(r.Context(), id)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "draft_not_found"})
		return nil, nil, false
	}
	if err != nil {
		// 잘못된 uuid 형식도 여기로 온다 → 404로 통일.
		if strings.Contains(err.Error(), "invalid input syntax") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "draft_not_found"})
			return nil, nil, false
		}
		s.logger.Error(logTag+": load failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return nil, nil, false
	}
	if d.CompanyProfileID != profile.ID {
		s.logger.Info("proposal draft access denied (ownership)", "draftId", id, "profileId", profile.ID)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "draft_not_found"})
		return nil, nil, false
	}
	return d, profile, true
}

// GET /api/proposal-drafts — 우리 회사 초안 목록(최신순, 최대 200). 2026-08-18 사용자 앱
// "제안서"(#/app/proposals) 허브 페이지용. 초안 본문(content)은 내려주지 않고 목록 메타만
// (id·공고·버전·stale·제목·시각). requireOwnedDraft와 같은 이유로 유료 게이트 없이 로그인+회사만
// (체험/강등 후 초안도 목록에 보여야 한다). 소유 회사 스코프 외 데이터는 절대 포함하지 않는다.
func (s *Server) handleListProposalDrafts(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("proposal-drafts-list: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0})
		return
	}
	ctx := r.Context()
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.notice_id, d.notice_version_id, d.status, d.title, d.generated_at, d.created_at, d.updated_at,
		       n.title, n.organization_name, n.current_version, n.application_end_at,
		       dv.version_number,
		       (SELECT id FROM notice_versions cv WHERE cv.notice_id = n.id AND cv.version_number = n.current_version LIMIT 1) AS current_version_id
		FROM proposal_drafts d
		JOIN notices n ON n.id = d.notice_id
		LEFT JOIN notice_versions dv ON dv.id = d.notice_version_id
		WHERE d.company_profile_id = $1
		ORDER BY d.updated_at DESC
		LIMIT 200`, profile.ID)
	if err != nil {
		s.logger.Error("proposal-drafts-list: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, noticeID, versionID, status, title, noticeTitle string
		var org, currentVersionID sql.NullString
		var generatedAt sql.NullTime
		var appEnd sql.NullTime
		var createdAt, updatedAt time.Time
		var currentVersion int
		var draftVersion sql.NullInt64
		if err := rows.Scan(&id, &noticeID, &versionID, &status, &title, &generatedAt, &createdAt, &updatedAt,
			&noticeTitle, &org, &currentVersion, &appEnd, &draftVersion, &currentVersionID); err != nil {
			s.logger.Error("proposal-drafts-list: scan failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		stale := currentVersionID.Valid && currentVersionID.String != versionID
		var genAt, endAt *time.Time
		if generatedAt.Valid {
			t := generatedAt.Time
			genAt = &t
		}
		if appEnd.Valid {
			t := appEnd.Time
			endAt = &t
		}
		items = append(items, map[string]any{
			"id":                   id,
			"noticeId":             noticeID,
			"noticeTitle":          noticeTitle,
			"organizationName":     org.String,
			"applicationEndAt":     endAt,
			"noticeVersionNumber":  draftVersion.Int64,
			"currentVersionNumber": currentVersion,
			"stale":                stale,
			"status":               status,
			"title":                title,
			"generatedAt":          genAt,
			"createdAt":            createdAt,
			"updatedAt":            updatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

// GET /api/proposal-drafts/{id}
func (s *Server) handleGetProposalDraft(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.requireOwnedDraft(w, r, "proposal-draft-get")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.draftDTO(r.Context(), d))
}

// PATCH /api/proposal-drafts/{id}  {title?, sections:[{id, body?, bullets?}]}
func (s *Server) handlePatchProposalDraft(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.requireOwnedDraft(w, r, "proposal-draft-patch")
	if !ok {
		return
	}
	var body struct {
		Title    *string `json:"title"`
		Sections []struct {
			ID      string    `json:"id"`
			Body    *string   `json:"body"`
			Bullets *[]string `json:"bullets"`
		} `json:"sections"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if body.Title != nil {
		t := strings.TrimSpace(*body.Title)
		if t != "" {
			d.Title = truncateRunes(t, 200)
			d.Content.Title = d.Title
		}
	}
	for _, in := range body.Sections {
		for i := range d.Content.Sections {
			if d.Content.Sections[i].ID != in.ID {
				continue
			}
			if in.Body != nil {
				d.Content.Sections[i].Body = truncateRunes(*in.Body, 20000)
			}
			if in.Bullets != nil {
				bl := make([]string, 0, len(*in.Bullets))
				for _, b := range *in.Bullets {
					if strings.TrimSpace(b) != "" {
						bl = append(bl, truncateRunes(b, 2000))
					}
				}
				d.Content.Sections[i].Bullets = bl
			}
		}
	}
	contJSON, _ := json.Marshal(d.Content)
	if _, err := s.db.ExecContext(r.Context(), `UPDATE proposal_drafts SET title = $2, content = $3::jsonb, updated_at = now() WHERE id = $1`, d.ID, d.Title, string(contJSON)); err != nil {
		s.logger.Error("proposal-draft-patch: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	d2, err := s.loadProposalDraft(r.Context(), d.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, s.draftDTO(r.Context(), d2))
}

// GET /api/proposal-drafts/{id}/docx
func (s *Server) handleProposalDraftDocx(w http.ResponseWriter, r *http.Request) {
	d, _, ok := s.requireOwnedDraft(w, r, "proposal-draft-docx")
	if !ok {
		return
	}
	ctx := r.Context()
	notice, err := s.loadProposalNoticeFacts(ctx, d.NoticeID)
	if err != nil {
		s.logger.Error("proposal-draft-docx: notice lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	facts := d.Company
	if facts == nil {
		facts = &companyFacts{}
	}
	at := d.CreatedAt
	if d.GeneratedAt.Valid {
		at = d.GeneratedAt.Time
	}
	b, err := buildProposalDocx(d.Content, notice, facts, at)
	if err != nil {
		s.logger.Error("proposal docx generation failed", "error", err, "draftId", d.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "docx_failed"})
		return
	}
	s.logger.Info("proposal docx generated", "draftId", d.ID, "bytes", len(b))
	filename := proposalDocxFilename(notice.Title, facts.CompanyName, time.Now())
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	w.Header().Set("Content-Disposition", `attachment; filename="proposal.docx"; filename*=UTF-8''`+url.PathEscape(filename))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = bytes.NewReader(b).WriteTo(w)
}
