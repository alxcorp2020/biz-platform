// proposal_mapping.go — 평가기준 맞춤 제안서 1차(2026-08-16): 회사 canonical
// 사실(facts) 로더 + 평가항목↔회사정보 대응표(작성 준비 화면) + 부족정보 질문.
//
// 회사정보는 반드시 기존 canonical 테이블에서만 읽는다(company_profiles /
// company_track_records / company_personnel / company_licenses /
// company_certifications / company_intellectual_property / company_financials /
// direct_production_status). 여기 없는 사실은 절대 만들지 않는다 — 없으면
// "추가 확인 필요" + 질문(정보가 없는 것만) 이고, 답이 없으면 초안에 [확인 필요]로 남는다.
// 사용자가 질문에 답한 값은 초안(proposal_drafts.answers)에만 저장하고 회사정보에
// 자동 영구저장하지 않는다.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ---- 회사 사실(snapshot) ----

type trackRecordFact struct {
	ProjectName    string  `json:"projectName"`
	ClientName     string  `json:"clientName,omitempty"`
	PeriodStart    string  `json:"periodStart,omitempty"`
	PeriodEnd      string  `json:"periodEnd,omitempty"`
	ContractAmount *int64  `json:"contractAmount,omitempty"`
	ProjectType    string  `json:"projectType,omitempty"`
	IndustryField  string  `json:"industryField,omitempty"`
	Scope          string  `json:"scope,omitempty"`
	CoreTechnology string  `json:"coreTechnology,omitempty"`
	IsCompleted    *bool   `json:"isCompleted,omitempty"`
	IsJointVenture *bool   `json:"isJointVenture,omitempty"`
	ShareRatio     *string `json:"shareRatio,omitempty"`
}

type personnelFact struct {
	Role           string   `json:"role"`
	TechField      string   `json:"techField,omitempty"`
	CareerYears    *float64 `json:"careerYears,omitempty"`
	TechGrade      string   `json:"techGrade,omitempty"`
	Qualifications []string `json:"qualifications"`
	RecentProject  string   `json:"recentProject,omitempty"`
}

type credentialFact struct {
	Kind               string `json:"kind"` // license | certification | ip
	Category           string `json:"category,omitempty"`
	Name               string `json:"name"`
	RegistrationNumber string `json:"registrationNumber,omitempty"`
	IssuingAuthority   string `json:"issuingAuthority,omitempty"`
	IssuedAt           string `json:"issuedAt,omitempty"`
	ExpiresAt          string `json:"expiresAt,omitempty"`
	Status             string `json:"status,omitempty"`
}

type financialFact struct {
	FiscalYear      int     `json:"fiscalYear"`
	Revenue         *int64  `json:"revenue,omitempty"`
	OperatingProfit *int64  `json:"operatingProfit,omitempty"`
	NetIncome       *int64  `json:"netIncome,omitempty"`
	DebtRatio       *string `json:"debtRatio,omitempty"`
	CreditRating    string  `json:"creditRating,omitempty"`
}

// companyFacts — 초안 생성 당시 회사정보 스냅샷(proposal_drafts.company_snapshot).
type companyFacts struct {
	ProfileID              string            `json:"profileId"`
	CompanyName            string            `json:"companyName,omitempty"`
	RepresentativeName     string            `json:"representativeName,omitempty"`
	Address                string            `json:"address,omitempty"`
	Region                 string            `json:"region,omitempty"`
	BusinessType           []string          `json:"businessType"`
	Industry               []string          `json:"industry"`
	FoundingDate           string            `json:"foundingDate,omitempty"`
	BusinessAgeYears       *float64          `json:"businessAgeYears,omitempty"`
	EmployeeCount          *int64            `json:"employeeCount,omitempty"`
	RevenueAmount          *int64            `json:"revenueAmount,omitempty"`
	CompanySize            string            `json:"companySize,omitempty"`
	CreditRating           string            `json:"creditRating,omitempty"`
	DirectProductionStatus string            `json:"directProductionStatus,omitempty"`
	TrackRecords           []trackRecordFact `json:"trackRecords"`
	Personnel              []personnelFact   `json:"personnel"`
	Credentials            []credentialFact  `json:"credentials"`
	Financials             []financialFact   `json:"financials"`
	SnapshotAt             time.Time         `json:"snapshotAt"`
}

func dateStr(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format("2006-01-02")
}

// loadCompanyFacts — canonical 테이블에서만 읽는다. 없는 테이블/행은 빈 슬라이스.
func (s *Server) loadCompanyFacts(ctx context.Context, profileID string) (*companyFacts, error) {
	f := &companyFacts{ProfileID: profileID, BusinessType: []string{}, Industry: []string{}, TrackRecords: []trackRecordFact{}, Personnel: []personnelFact{}, Credentials: []credentialFact{}, Financials: []financialFact{}, SnapshotAt: time.Now()}
	var name, rep, addr, region, size, credit, dps sql.NullString
	var founding sql.NullTime
	var age sql.NullFloat64
	var emp, rev sql.NullInt64
	var bt, ind pq.StringArray
	err := s.db.QueryRowContext(ctx, `SELECT company_name, representative_name, address, region, company_size, credit_rating, direct_production_status,
			founding_date, business_age_years, employee_count, revenue_amount, business_type, industry
		FROM company_profiles WHERE id = $1`, profileID).Scan(&name, &rep, &addr, &region, &size, &credit, &dps, &founding, &age, &emp, &rev, &bt, &ind)
	if err != nil {
		return nil, err
	}
	f.CompanyName, f.RepresentativeName, f.Address, f.Region = name.String, rep.String, addr.String, region.String
	f.CompanySize, f.CreditRating, f.DirectProductionStatus = size.String, credit.String, dps.String
	f.FoundingDate = dateStr(founding)
	if age.Valid {
		f.BusinessAgeYears = &age.Float64
	}
	if emp.Valid {
		f.EmployeeCount = &emp.Int64
	}
	if rev.Valid {
		f.RevenueAmount = &rev.Int64
	}
	if bt != nil {
		f.BusinessType = []string(bt)
	}
	if ind != nil {
		f.Industry = []string(ind)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT project_name, client_name, period_start, period_end, contract_amount, project_type, industry_field, scope, core_technology, is_completed, is_joint_venture, share_ratio
		FROM company_track_records WHERE company_profile_id = $1 ORDER BY period_end DESC NULLS LAST, created_at DESC`, profileID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t trackRecordFact
		var client, ptype, field, scope, tech sql.NullString
		var ps, pe sql.NullTime
		var amt sql.NullInt64
		var done, jv sql.NullBool
		var share sql.NullString
		if err := rows.Scan(&t.ProjectName, &client, &ps, &pe, &amt, &ptype, &field, &scope, &tech, &done, &jv, &share); err != nil {
			rows.Close()
			return nil, err
		}
		t.ClientName, t.ProjectType, t.IndustryField, t.Scope, t.CoreTechnology = client.String, ptype.String, field.String, scope.String, tech.String
		t.PeriodStart, t.PeriodEnd = dateStr(ps), dateStr(pe)
		if amt.Valid {
			t.ContractAmount = &amt.Int64
		}
		if done.Valid {
			t.IsCompleted = &done.Bool
		}
		if jv.Valid {
			t.IsJointVenture = &jv.Bool
		}
		if share.Valid {
			t.ShareRatio = &share.String
		}
		f.TrackRecords = append(f.TrackRecords, t)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT role, tech_field, career_years, tech_grade, qualifications, recent_project FROM company_personnel WHERE company_profile_id = $1 ORDER BY created_at`, profileID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p personnelFact
		var role, field, grade, recent sql.NullString
		var years sql.NullFloat64
		var quals pq.StringArray
		if err := rows.Scan(&role, &field, &years, &grade, &quals, &recent); err != nil {
			rows.Close()
			return nil, err
		}
		p.Role, p.TechField, p.TechGrade, p.RecentProject = role.String, field.String, grade.String, recent.String
		if years.Valid {
			p.CareerYears = &years.Float64
		}
		p.Qualifications = []string(quals)
		if p.Qualifications == nil {
			p.Qualifications = []string{}
		}
		f.Personnel = append(f.Personnel, p)
	}
	rows.Close()

	for _, src := range []struct{ table, kind string }{{"company_licenses", "license"}, {"company_certifications", "certification"}} {
		rows, err = s.db.QueryContext(ctx, `SELECT category, name, registration_number, issuing_authority, issued_at, expires_at, status FROM `+src.table+` WHERE company_profile_id = $1 ORDER BY created_at`, profileID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c credentialFact
			var cat, reg, auth, status sql.NullString
			var issued, expires sql.NullTime
			if err := rows.Scan(&cat, &c.Name, &reg, &auth, &issued, &expires, &status); err != nil {
				rows.Close()
				return nil, err
			}
			c.Kind, c.Category, c.RegistrationNumber, c.IssuingAuthority, c.Status = src.kind, cat.String, reg.String, auth.String, status.String
			c.IssuedAt, c.ExpiresAt = dateStr(issued), dateStr(expires)
			f.Credentials = append(f.Credentials, c)
		}
		rows.Close()
	}
	rows, err = s.db.QueryContext(ctx, `SELECT ip_type, title, registration_number, registration_date, status FROM company_intellectual_property WHERE company_profile_id = $1 ORDER BY created_at`, profileID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c credentialFact
		var ipType, reg, status sql.NullString
		var regDate sql.NullTime
		if err := rows.Scan(&ipType, &c.Name, &reg, &regDate, &status); err != nil {
			rows.Close()
			return nil, err
		}
		c.Kind, c.Category, c.RegistrationNumber, c.Status = "ip", ipType.String, reg.String, status.String
		c.IssuedAt = dateStr(regDate)
		f.Credentials = append(f.Credentials, c)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT fiscal_year, revenue, operating_profit, net_income, debt_ratio, credit_rating FROM company_financials WHERE company_profile_id = $1 ORDER BY fiscal_year DESC`, profileID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fin financialFact
		var rev, op, ni sql.NullInt64
		var debt, credit sql.NullString
		if err := rows.Scan(&fin.FiscalYear, &rev, &op, &ni, &debt, &credit); err != nil {
			rows.Close()
			return nil, err
		}
		if rev.Valid {
			fin.Revenue = &rev.Int64
		}
		if op.Valid {
			fin.OperatingProfit = &op.Int64
		}
		if ni.Valid {
			fin.NetIncome = &ni.Int64
		}
		if debt.Valid {
			fin.DebtRatio = &debt.String
		}
		fin.CreditRating = credit.String
		f.Financials = append(f.Financials, fin)
	}
	rows.Close()
	return f, nil
}

// ---- 평가항목 ↔ 회사정보 대응 ----

const (
	kindUnderstanding = "understanding"
	kindPlan          = "plan"
	kindTrackRecord   = "track_record"
	kindPersonnel     = "personnel"
	kindCredential    = "credential"
	kindFinancial     = "financial"
	kindManagement    = "management"
	kindPrice         = "price"
	kindOther         = "other"

	readyStatusReady   = "ready"
	readyStatusPartial = "ready_partial"
	readyStatusInput   = "needs_input"
	readyStatusNA      = "not_applicable"
)

var readinessStatusLabels = map[string]string{
	readyStatusReady:   "작성 준비 완료",
	readyStatusPartial: "작성 준비 완료 · 일부 확인 필요",
	readyStatusInput:   "추가 확인 필요",
	readyStatusNA:      "별도 제출",
}

type questionOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// readinessQuestion — 정보가 없는 것만 묻는다. 답변은 초안 answers에만 저장.
type readinessQuestion struct {
	ID            string           `json:"id"`
	CriterionID   string           `json:"criterionId"`
	Prompt        string           `json:"prompt"`
	Options       []questionOption `json:"options"`
	AllowFreeText bool             `json:"allowFreeText"`
	FreeTextHint  string           `json:"freeTextHint,omitempty"`
	LinkLabel     string           `json:"linkLabel,omitempty"`
	LinkHref      string           `json:"linkHref,omitempty"`
}

type readinessItem struct {
	CriterionID string             `json:"criterionId"`
	Title       string             `json:"title"`
	Score       *float64           `json:"score"`
	Category    string             `json:"category"`
	Kind        string             `json:"kind"`
	Status      string             `json:"status"`
	StatusLabel string             `json:"statusLabel"`
	Evidence    []string           `json:"evidence"`
	Question    *readinessQuestion `json:"question,omitempty"`
}

type readinessSummary struct {
	CriteriaCount int      `json:"criteriaCount"`
	TotalScore    *float64 `json:"totalScore"`
	ReadyCount    int      `json:"readyCount"`
	NeedsInput    int      `json:"needsInputCount"`
}

// classifyCriterionKind — 제목+세부기준 키워드로 회사정보 종류를 정한다.
func classifyCriterionKind(c evaluationCriterion) string {
	t := strings.ReplaceAll(c.Title+" "+strings.Join(c.SubCriteria, " ")+" "+c.Description, " ", "")
	title := strings.ReplaceAll(c.Title, " ", "")
	has := func(s string, kws ...string) bool {
		for _, k := range kws {
			if strings.Contains(s, k) {
				return true
			}
		}
		return false
	}
	switch {
	case c.Category == "price" || has(title, "가격", "입찰금액", "제안가"):
		return kindPrice
	case has(title, "실적", "수행경험", "유사사업", "수행사업", "경험"):
		return kindTrackRecord
	case has(title, "인력", "조직", "책임자", "투입", "기술자", "전문가", "수행조직", "참여인력", "PM", "인적"):
		return kindPersonnel
	case has(title, "인증", "특허", "지식재산", "기술보유", "품질보증", "ISO", "자격"):
		return kindCredential
	case has(title, "경영", "재무", "신용", "매출", "재정", "정량"):
		return kindFinancial
	case has(title, "사후", "유지보수", "하자", "A/S", "AS", "지원체계", "비상", "안전", "보안", "위험", "리스크"):
		return kindManagement
	case has(title, "이해", "목적", "배경", "필요성", "이해도"):
		return kindUnderstanding
	case has(title, "계획", "방법", "전략", "추진", "일정", "방법론", "수행방안", "체계", "품질", "관리", "운영", "기획", "구성", "내용", "적정", "타당", "창의", "독창", "효과", "성과", "활용", "확산", "홍보", "디자인", "설계", "검증"):
		return kindPlan
	case has(t, "실적"):
		return kindTrackRecord
	case has(t, "인력", "조직", "책임자", "경력"):
		return kindPersonnel
	}
	return kindOther
}

// buildReadiness — 평가기준 × 회사 사실 → 항목별 상태/근거/질문 + 요약.
func buildReadiness(set *evaluationCriteriaSet, facts *companyFacts) ([]readinessItem, []readinessQuestion, readinessSummary) {
	items := make([]readinessItem, 0, len(set.Criteria))
	var questions []readinessQuestion
	sum := readinessSummary{CriteriaCount: len(set.Criteria), TotalScore: set.TotalScore}
	for _, c := range set.Criteria {
		kind := classifyCriterionKind(c)
		it := readinessItem{CriterionID: c.ID, Title: c.Title, Score: c.Score, Category: c.Category, Kind: kind, Evidence: []string{}}
		var q *readinessQuestion
		switch kind {
		case kindTrackRecord:
			if n := len(facts.TrackRecords); n > 0 {
				it.Status = readyStatusReady
				it.Evidence = append(it.Evidence, fmt.Sprintf("등록된 수행실적 %d건 반영", n))
			} else {
				it.Status = readyStatusInput
				it.Evidence = append(it.Evidence, "등록된 수행실적 없음")
				q = &readinessQuestion{ID: "q_" + c.ID, CriterionID: c.ID, Prompt: "이 항목에 반영할 수행실적이 등록되어 있지 않습니다. 실적을 등록하면 초안에 그대로 반영됩니다.",
					Options: []questionOption{{Value: "later", Label: "이번 초안에서는 [확인 필요]로 남기기"}}, LinkLabel: "실적 등록하기", LinkHref: "#/me/company"}
			}
		case kindPersonnel:
			n := len(facts.Personnel)
			detailed := 0
			for _, p := range facts.Personnel {
				if p.CareerYears != nil || p.RecentProject != "" || len(p.Qualifications) > 0 {
					detailed++
				}
			}
			opts := []questionOption{}
			for i, p := range facts.Personnel {
				label := p.Role
				if p.TechField != "" {
					label += " · " + p.TechField
				}
				if p.CareerYears != nil {
					label += fmt.Sprintf(" · 경력 %.0f년", *p.CareerYears)
				}
				opts = append(opts, questionOption{Value: "personnel:" + strconv.Itoa(i), Label: label})
			}
			q = &readinessQuestion{ID: "q_" + c.ID, CriterionID: c.ID, Prompt: "이 사업의 책임자(PM)는 누구인가요?", Options: opts, AllowFreeText: true, FreeTextHint: "이름/직위/관련 경력(연수)·주요 수행사업을 직접 입력"}
			if n == 0 {
				it.Status = readyStatusInput
				it.Evidence = append(it.Evidence, "등록된 인력 정보 없음")
			} else if detailed < n {
				it.Status = readyStatusPartial
				it.Evidence = append(it.Evidence, fmt.Sprintf("등록 인력 %d명 반영 · 경력 세부정보 부족 %d명", n, n-detailed))
			} else {
				it.Status = readyStatusPartial
				it.Evidence = append(it.Evidence, fmt.Sprintf("등록 인력 %d명 반영", n))
			}
		case kindCredential:
			if n := len(facts.Credentials); n > 0 {
				it.Status = readyStatusReady
				it.Evidence = append(it.Evidence, fmt.Sprintf("등록된 면허·인증·지식재산 %d건 반영", n))
			} else {
				it.Status = readyStatusInput
				it.Evidence = append(it.Evidence, "등록된 면허·인증·지식재산 없음")
				q = &readinessQuestion{ID: "q_" + c.ID, CriterionID: c.ID, Prompt: "이 항목에 반영할 인증·특허·자격이 있나요?",
					Options: []questionOption{{Value: "none", Label: "해당 없음"}, {Value: "later", Label: "이번 초안에서는 [확인 필요]로 남기기"}}, AllowFreeText: true, FreeTextHint: "인증/특허명과 등록번호를 직접 입력", LinkLabel: "면허·인증 등록하기", LinkHref: "#/me/company"}
			}
		case kindFinancial:
			if len(facts.Financials) > 0 || facts.CreditRating != "" {
				it.Status = readyStatusReady
				if len(facts.Financials) > 0 {
					it.Evidence = append(it.Evidence, fmt.Sprintf("등록된 재무정보 %d개년 반영", len(facts.Financials)))
				}
				if facts.CreditRating != "" {
					it.Evidence = append(it.Evidence, "신용평가등급 반영")
				}
			} else {
				it.Status = readyStatusInput
				it.Evidence = append(it.Evidence, "등록된 재무·신용 정보 없음")
				q = &readinessQuestion{ID: "q_" + c.ID, CriterionID: c.ID, Prompt: "신용평가등급 또는 최근 결산 매출액을 알려주세요.",
					Options: []questionOption{{Value: "later", Label: "이번 초안에서는 [확인 필요]로 남기기"}}, AllowFreeText: true, FreeTextHint: "예: 신용평가등급 BBB0 / 2025년 매출 12억원", LinkLabel: "재무정보 등록하기", LinkHref: "#/me/company"}
			}
		case kindManagement:
			it.Status = readyStatusInput
			it.Evidence = append(it.Evidence, "운영·안전·사후관리 방식은 회사정보에 없는 항목")
			q = &readinessQuestion{ID: "q_" + c.ID, CriterionID: c.ID, Prompt: fmt.Sprintf("'%s' 항목의 운영·대응 체계(대응 시간, 담당 조직 등)를 알려주세요.", c.Title),
				Options: []questionOption{{Value: "business_hours", Label: "평일 업무시간 대응"}, {Value: "24h", Label: "24시간 대응"}}, AllowFreeText: true, FreeTextHint: "대응 시간, 담당 조직, 하자보수·비상대응 절차 등을 직접 입력"}
		case kindUnderstanding, kindPlan:
			it.Status = readyStatusReady
			it.Evidence = append(it.Evidence, "공고자료 기반으로 작성 구조 구성(내용은 제출 전 직접 보완)")
		case kindPrice:
			it.Status = readyStatusNA
			it.Evidence = append(it.Evidence, "가격 제안은 입찰서(가격 제안서)로 별도 제출")
		default:
			it.Status = readyStatusInput
			it.Evidence = append(it.Evidence, "회사정보와 자동으로 연결되지 않는 항목")
			q = &readinessQuestion{ID: "q_" + c.ID, CriterionID: c.ID, Prompt: fmt.Sprintf("'%s' 항목에 반영할 회사 내용을 알려주세요.", c.Title),
				Options: []questionOption{{Value: "later", Label: "이번 초안에서는 [확인 필요]로 남기기"}}, AllowFreeText: true, FreeTextHint: "이 항목에 쓸 사실만 간단히"}
		}
		it.StatusLabel = readinessStatusLabels[it.Status]
		if q != nil {
			it.Question = q
			questions = append(questions, *q)
		}
		switch it.Status {
		case readyStatusReady, readyStatusPartial:
			sum.ReadyCount++
		case readyStatusInput:
			sum.NeedsInput++
		}
		items = append(items, it)
	}
	if questions == nil {
		questions = []readinessQuestion{}
	}
	return items, questions, sum
}
