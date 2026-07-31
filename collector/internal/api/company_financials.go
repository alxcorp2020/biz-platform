// 재무정보 — company_licenses/certifications와 같은 패턴(출처+신뢰도
// A~D+증빙연결)으로 구현하되, 연도별 1행이라는 특성상 status 컬럼이
// 없고 fiscal_year 기준 upsert다(같은 연도를 다시 저장하면 갱신).
package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

var numericLikePattern = regexp.MustCompile(`^-?[\d,]+(\.\d+)?$`)

type financialCandidate struct {
	FiscalYear        string `json:"fiscalYear"`
	Revenue           string `json:"revenue"`
	OperatingProfit   string `json:"operatingProfit"`
	NetIncome         string `json:"netIncome"`
	Capital           string `json:"capital"`
	TotalAssets       string `json:"totalAssets"`
	TotalLiabilities  string `json:"totalLiabilities"`
	DebtRatio         string `json:"debtRatio"`
	CurrentRatio      string `json:"currentRatio"`
	CreditRating      string `json:"creditRating"`
	TaxDelinquent     string `json:"taxDelinquent"`
	CapitalImpairment string `json:"capitalImpairment"`
}

func (s *Server) handleUploadFinancialDocument(w http.ResponseWriter, r *http.Request) {
	_, documentID, body, ext, mediaType, ok := s.receiveCompanyDocument(w, r)
	if !ok {
		return
	}
	candidate, err := s.extractFinancialCandidate(r.Context(), body, ext, mediaType)
	if err != nil {
		s.logger.Error("upload-financial-document: claude extraction failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "extraction_failed", "documentId": documentID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documentId": documentID, "candidate": candidate})
}

func (s *Server) extractFinancialCandidate(ctx context.Context, body []byte, ext, mediaType string) (*financialCandidate, error) {
	b64 := base64.StdEncoding.EncodeToString(body)
	var docBlock anthropic.ContentBlockParamUnion
	if ext == "pdf" {
		docBlock = anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{Data: b64})
	} else {
		docBlock = anthropic.NewImageBlockBase64(mediaType, b64)
	}

	triStateEnum := []string{"예", "아니오", "확인불가"}
	tool := anthropic.ToolParam{
		Name: "extract_financial_fields",
		Description: anthropic.String(
			"업로드된 재무제표/재무증빙서류(재무제표, 신용평가서, 표준재무제표증명, 부가가치세 과세표준증명 등)에서 " +
				"실제로 문서에 적혀 있는 수치만 추출합니다. " +
				"문서에 없는 내용은 절대 만들어내지 마세요. 확인할 수 없는 필드는 빈 문자열로 두세요.",
		),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"fiscalYear":        map[string]any{"type": "string", "description": "회계연도(YYYY). 없으면 빈 문자열"},
				"revenue":           map[string]any{"type": "string", "description": "매출액(숫자만, 원 단위). 없으면 빈 문자열"},
				"operatingProfit":   map[string]any{"type": "string", "description": "영업이익(숫자만, 음수 가능). 없으면 빈 문자열"},
				"netIncome":         map[string]any{"type": "string", "description": "당기순이익(숫자만, 음수 가능). 없으면 빈 문자열"},
				"capital":           map[string]any{"type": "string", "description": "자본금(숫자만). 없으면 빈 문자열"},
				"totalAssets":       map[string]any{"type": "string", "description": "자산총계(숫자만). 없으면 빈 문자열"},
				"totalLiabilities":  map[string]any{"type": "string", "description": "부채총계(숫자만). 없으면 빈 문자열"},
				"debtRatio":         map[string]any{"type": "string", "description": "부채비율(%, 숫자만). 없으면 빈 문자열"},
				"currentRatio":      map[string]any{"type": "string", "description": "유동비율(%, 숫자만). 없으면 빈 문자열"},
				"creditRating":      map[string]any{"type": "string", "description": "신용등급. 없으면 빈 문자열"},
				"taxDelinquent":     map[string]any{"type": "string", "description": "세금체납 여부", "enum": triStateEnum},
				"capitalImpairment": map[string]any{"type": "string", "description": "자본잠식 여부", "enum": triStateEnum},
			},
			Required: []string{
				"fiscalYear", "revenue", "operatingProfit", "netIncome", "capital", "totalAssets",
				"totalLiabilities", "debtRatio", "currentRatio", "creditRating", "taxDelinquent", "capitalImpairment",
			},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}

	resp, err := s.anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:      companyDocumentModel,
		MaxTokens:  1024,
		Tools:      []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice: anthropic.ToolChoiceParamOfTool("extract_financial_fields"),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(docBlock, anthropic.NewTextBlock("이 재무 증빙서류에서 정보를 추출하세요.")),
		},
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return nil, fmt.Errorf("claude api error (status %d): %w", apiErr.StatusCode, err)
		}
		return nil, err
	}

	for _, block := range resp.Content {
		if tu, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
			var candidate financialCandidate
			if err := json.Unmarshal(tu.Input, &candidate); err != nil {
				return nil, fmt.Errorf("parse tool input: %w", err)
			}
			sanitizeFinancialCandidate(&candidate)
			return &candidate, nil
		}
	}
	return nil, fmt.Errorf("no tool_use block in response (stop_reason=%s)", resp.StopReason)
}

func sanitizeFinancialCandidate(c *financialCandidate) {
	if c.FiscalYear != "" && !regexp.MustCompile(`^\d{4}$`).MatchString(c.FiscalYear) {
		c.FiscalYear = ""
	}
	for _, f := range []*string{
		&c.Revenue, &c.OperatingProfit, &c.NetIncome, &c.Capital,
		&c.TotalAssets, &c.TotalLiabilities, &c.DebtRatio, &c.CurrentRatio,
	} {
		if *f != "" && !numericLikePattern.MatchString(*f) {
			*f = ""
		}
	}
	if looksMalformed(c.CreditRating) {
		c.CreditRating = ""
	}
	if c.TaxDelinquent != "" && c.TaxDelinquent != "예" && c.TaxDelinquent != "아니오" && c.TaxDelinquent != "확인불가" {
		c.TaxDelinquent = ""
	}
	if c.CapitalImpairment != "" && c.CapitalImpairment != "예" && c.CapitalImpairment != "아니오" && c.CapitalImpairment != "확인불가" {
		c.CapitalImpairment = ""
	}
}

type financialRequest struct {
	FiscalYear        int     `json:"fiscalYear"`
	Revenue           *string `json:"revenue"`
	OperatingProfit   *string `json:"operatingProfit"`
	NetIncome         *string `json:"netIncome"`
	Capital           *string `json:"capital"`
	TotalAssets       *string `json:"totalAssets"`
	TotalLiabilities  *string `json:"totalLiabilities"`
	DebtRatio         *string `json:"debtRatio"`
	CurrentRatio      *string `json:"currentRatio"`
	CreditRating      *string `json:"creditRating"`
	TaxDelinquent     *string `json:"taxDelinquent"`
	CapitalImpairment *string `json:"capitalImpairment"`
	SourceDocumentID  *string `json:"sourceDocumentId"`
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Server) handleCreateFinancial(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("create-financial: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	var req financialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if req.FiscalYear < 1900 || req.FiscalYear > 2200 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_fiscal_year"})
		return
	}

	revenue, err1 := parseOptionalInt64(strOrEmpty(req.Revenue))
	operatingProfit, err2 := parseOptionalInt64(strOrEmpty(req.OperatingProfit))
	netIncome, err3 := parseOptionalInt64(strOrEmpty(req.NetIncome))
	capital, err4 := parseOptionalInt64(strOrEmpty(req.Capital))
	totalAssets, err5 := parseOptionalInt64(strOrEmpty(req.TotalAssets))
	totalLiabilities, err6 := parseOptionalInt64(strOrEmpty(req.TotalLiabilities))
	debtRatio, err7 := parseOptionalFloat64(strOrEmpty(req.DebtRatio))
	currentRatio, err8 := parseOptionalFloat64(strOrEmpty(req.CurrentRatio))
	taxDelinquent, err9 := parseTriStateBool(strOrEmpty(req.TaxDelinquent))
	capitalImpairment, err10 := parseTriStateBool(strOrEmpty(req.CapitalImpairment))
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil ||
		err6 != nil || err7 != nil || err8 != nil || err9 != nil || err10 != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field_format"})
		return
	}

	confidence := "C"
	if req.SourceDocumentID != nil && strings.TrimSpace(*req.SourceDocumentID) != "" {
		var owns bool
		err := s.db.QueryRowContext(r.Context(),
			`SELECT EXISTS (SELECT 1 FROM company_documents WHERE id = $1 AND company_profile_id = $2)`,
			*req.SourceDocumentID, profile.ID,
		).Scan(&owns)
		if err != nil {
			s.logger.Error("create-financial: source document check failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		if !owns {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_source_document"})
			return
		}
		confidence = "B"
	}

	var id string
	err = s.db.QueryRowContext(r.Context(), `
		INSERT INTO company_financials (
			company_profile_id, fiscal_year, revenue, operating_profit, net_income, capital,
			total_assets, total_liabilities, debt_ratio, current_ratio, credit_rating,
			tax_delinquent, capital_impairment, source_document_id, confidence, verified_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15, now())
		ON CONFLICT (company_profile_id, fiscal_year) DO UPDATE SET
			revenue = $3, operating_profit = $4, net_income = $5, capital = $6,
			total_assets = $7, total_liabilities = $8, debt_ratio = $9, current_ratio = $10,
			credit_rating = $11, tax_delinquent = $12, capital_impairment = $13,
			source_document_id = $14, confidence = $15, verified_at = now(), updated_at = now()
		RETURNING id`,
		profile.ID, req.FiscalYear, revenue, operatingProfit, netIncome, capital,
		totalAssets, totalLiabilities, debtRatio, currentRatio, req.CreditRating,
		taxDelinquent, capitalImpairment, req.SourceDocumentID, confidence,
	).Scan(&id)
	if err != nil {
		s.logger.Error("create-financial: upsert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "confidence": confidence})
}

type financialItem struct {
	ID                string     `json:"id"`
	FiscalYear        int        `json:"fiscalYear"`
	Revenue           *int64     `json:"revenue"`
	OperatingProfit   *int64     `json:"operatingProfit"`
	NetIncome         *int64     `json:"netIncome"`
	Capital           *int64     `json:"capital"`
	TotalAssets       *int64     `json:"totalAssets"`
	TotalLiabilities  *int64     `json:"totalLiabilities"`
	DebtRatio         *float64   `json:"debtRatio"`
	CurrentRatio      *float64   `json:"currentRatio"`
	CreditRating      *string    `json:"creditRating"`
	TaxDelinquent     *bool      `json:"taxDelinquent"`
	CapitalImpairment *bool      `json:"capitalImpairment"`
	SourceDocumentID  *string    `json:"sourceDocumentId"`
	Confidence        string     `json:"confidence"`
	VerifiedAt        *time.Time `json:"verifiedAt"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

func (s *Server) handleListFinancials(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-financials: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []financialItem{}})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, fiscal_year, revenue, operating_profit, net_income, capital, total_assets,
		       total_liabilities, debt_ratio, current_ratio, credit_rating, tax_delinquent,
		       capital_impairment, source_document_id, confidence, verified_at, created_at, updated_at
		FROM company_financials WHERE company_profile_id = $1 ORDER BY fiscal_year DESC`, profile.ID)
	if err != nil {
		s.logger.Error("list-financials: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []financialItem{}
	for rows.Next() {
		var it financialItem
		var revenue, operatingProfit, netIncome, capital, totalAssets, totalLiabilities sql.NullInt64
		var debtRatio, currentRatio sql.NullFloat64
		var creditRating, sourceDocID sql.NullString
		var taxDelinquent, capitalImpairment sql.NullBool
		var verifiedAt sql.NullTime
		if err := rows.Scan(&it.ID, &it.FiscalYear, &revenue, &operatingProfit, &netIncome, &capital,
			&totalAssets, &totalLiabilities, &debtRatio, &currentRatio, &creditRating, &taxDelinquent,
			&capitalImpairment, &sourceDocID, &it.Confidence, &verifiedAt, &it.CreatedAt, &it.UpdatedAt); err != nil {
			s.logger.Error("list-financials: scan failed", "error", err)
			continue
		}
		it.Revenue = nullInt64Ptr(revenue)
		it.OperatingProfit = nullInt64Ptr(operatingProfit)
		it.NetIncome = nullInt64Ptr(netIncome)
		it.Capital = nullInt64Ptr(capital)
		it.TotalAssets = nullInt64Ptr(totalAssets)
		it.TotalLiabilities = nullInt64Ptr(totalLiabilities)
		if debtRatio.Valid {
			it.DebtRatio = &debtRatio.Float64
		}
		if currentRatio.Valid {
			it.CurrentRatio = &currentRatio.Float64
		}
		it.CreditRating = nullStringPtr(creditRating)
		it.SourceDocumentID = nullStringPtr(sourceDocID)
		if taxDelinquent.Valid {
			it.TaxDelinquent = &taxDelinquent.Bool
		}
		if capitalImpairment.Valid {
			it.CapitalImpairment = &capitalImpairment.Bool
		}
		if verifiedAt.Valid {
			it.VerifiedAt = &verifiedAt.Time
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
