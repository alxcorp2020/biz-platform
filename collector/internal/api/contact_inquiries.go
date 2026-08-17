// contact_inquiries.go — 공개 "문의하기" 페이지(#/contact, 2026-08-18)의 백엔드.
//
// 이전엔 문의 접수 API/저장 구조가 없었다(랜딩 "제휴 문의" 카드는 이메일 주소만 노출).
// 가짜 성공 처리를 만들지 않기 위해 최소 저장 구조를 둔다:
//   - contact_inquiries 테이블(migrate.ensureContactInquiriesTable)에 접수 내용을 저장 —
//     운영자는 관리자 화면(#/admin/inquiries, GET /api/admin/inquiries)에서 확인한다.
//   - company_info.contact_email(없으면 partnership_email)이 설정돼 있고 Resend가 구성돼
//     있으면 접수 알림 메일을 best-effort로 보낸다(실패해도 접수 자체는 성공 — DB가 원본).
//   - 남용 방지: 같은 IP 1분 1회·1일 5회(auth_lookup_attempts kind='contact_inquiry' 재사용,
//     find_email/password_reset과 같은 기준). 로그인 여부 무관 공개 엔드포인트.
//   - 입력은 서버에서 길이·형식 검증 + trim. 화면 표시(관리자)는 프론트 escapeHtml, 메일은
//     html.EscapeString.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

// contactInquiryTypes — 문의 유형 선택지(프론트 <select>와 동일 값). 알 수 없는 값은 400.
var contactInquiryTypes = map[string]string{
	"service":     "서비스 이용 문의",
	"billing":     "요금제·결제 문의",
	"business":    "기업용(Business) 문의",
	"partnership": "제휴 문의",
	"bug":         "오류 신고",
	"other":       "기타",
}

const (
	contactInquiryMaxName    = 50
	contactInquiryMaxCompany = 100
	contactInquiryMaxEmail   = 254
	contactInquiryMaxPhone   = 30
	contactInquiryMaxMessage = 2000
	contactInquiryMinMessage = 10
)

type contactInquiryRequest struct {
	Name          string `json:"name"`
	CompanyName   string `json:"companyName"`
	Email         string `json:"email"`
	Phone         string `json:"phone"`
	InquiryType   string `json:"inquiryType"`
	Message       string `json:"message"`
	PrivacyAgreed bool   `json:"privacyAgreed"`
	// Website — 허니팟(화면에 안 보이는 필드). 봇이 채우면 조용히 성공 응답만 하고 저장 안 함.
	Website string `json:"website"`
}

// validateContactInquiry — 필수/형식/길이 검증. 실패 시 machine-readable 오류 코드 반환.
func validateContactInquiry(req *contactInquiryRequest) string {
	req.Name = strings.TrimSpace(req.Name)
	req.CompanyName = strings.TrimSpace(req.CompanyName)
	req.Email = strings.TrimSpace(req.Email)
	req.Phone = strings.TrimSpace(req.Phone)
	req.InquiryType = strings.TrimSpace(req.InquiryType)
	req.Message = strings.TrimSpace(req.Message)
	if req.Name == "" || req.Email == "" || req.InquiryType == "" || req.Message == "" {
		return "missing_required"
	}
	if utf8.RuneCountInString(req.Name) > contactInquiryMaxName ||
		utf8.RuneCountInString(req.CompanyName) > contactInquiryMaxCompany ||
		len(req.Email) > contactInquiryMaxEmail ||
		utf8.RuneCountInString(req.Phone) > contactInquiryMaxPhone {
		return "field_too_long"
	}
	if utf8.RuneCountInString(req.Message) > contactInquiryMaxMessage {
		return "message_too_long"
	}
	if utf8.RuneCountInString(req.Message) < contactInquiryMinMessage {
		return "message_too_short"
	}
	if addr, err := mail.ParseAddress(req.Email); err != nil || addr.Address != req.Email || !strings.Contains(req.Email, "@") {
		return "invalid_email"
	}
	if _, ok := contactInquiryTypes[req.InquiryType]; !ok {
		return "invalid_inquiry_type"
	}
	if !req.PrivacyAgreed {
		return "privacy_agreement_required"
	}
	return ""
}

// POST /api/contact — 공개. 성공 201 {id}. 오류: 400 코드, 429 rate_limited.
func (s *Server) handleCreateContactInquiry(w http.ResponseWriter, r *http.Request) {
	var req contactInquiryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if code := validateContactInquiry(&req); code != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
		return
	}
	// 허니팟: 봇 제출은 저장·메일 없이 성공처럼 응답(스팸이 재시도하지 않게).
	if strings.TrimSpace(req.Website) != "" {
		writeJSON(w, http.StatusCreated, map[string]any{"id": "", "accepted": true})
		return
	}
	ctx := r.Context()
	ip := clientIP(r)
	if ip == "" {
		ip = "unknown"
	}
	limited, err := authLookupRateLimited(ctx, s.db, "contact_inquiry", ip)
	if err != nil {
		s.logger.Error("contact-inquiry: rate limit lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if limited {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO auth_lookup_attempts (kind, identifier) VALUES ('contact_inquiry', $1)`, ip); err != nil {
		s.logger.Error("contact-inquiry: attempt log failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	// 로그인 상태면 계정을 함께 남긴다(선택 — 운영자 확인용). 탈퇴/하드삭제 시 SET NULL.
	var userID sql.NullString
	if uid, ok := s.currentUserID(r); ok {
		userID = sql.NullString{String: uid, Valid: true}
	}
	var id string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO contact_inquiries (name, company_name, email, phone, inquiry_type, message, privacy_agreed, user_id, client_ip)
		VALUES ($1, NULLIF($2::text,''), $3, NULLIF($4::text,''), $5, $6, TRUE, $7::uuid, NULLIF($8::text,''))
		RETURNING id`,
		req.Name, req.CompanyName, req.Email, req.Phone, req.InquiryType, req.Message, userID, ip,
	).Scan(&id)
	if err != nil {
		s.logger.Error("contact-inquiry: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	// 운영자 알림 메일(best-effort). 응답을 지연시키지 않도록 별도 goroutine + 자체 타임아웃.
	go s.notifyContactInquiry(id, req)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "accepted": true})
}

// notifyContactInquiry — company_info의 contact_email(없으면 partnership_email)로 접수 알림.
// Resend 미구성/수신 주소 없음이면 조용히 건너뛴다(DB 저장이 원본이라 유실 아님).
func (s *Server) notifyContactInquiry(id string, req contactInquiryRequest) {
	if s.notify == nil || !s.notify.Configured() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := s.fetchCompanyInfo(ctx)
	if err != nil {
		s.logger.Warn("contact-inquiry: company info lookup failed (skip email)", "error", err)
		return
	}
	to := ""
	if info.ContactEmail != nil && strings.TrimSpace(*info.ContactEmail) != "" {
		to = strings.TrimSpace(*info.ContactEmail)
	} else if info.PartnershipEmail != nil && strings.TrimSpace(*info.PartnershipEmail) != "" {
		to = strings.TrimSpace(*info.PartnershipEmail)
	}
	if to == "" {
		return
	}
	typeLabel := contactInquiryTypes[req.InquiryType]
	subject := fmt.Sprintf("[문의 접수] %s — %s", typeLabel, req.Name)
	body := fmt.Sprintf(`
<p>새 문의가 접수되었습니다. (관리자 화면 &gt; 문의 관리에서 확인)</p>
<table cellpadding="4">
<tr><td><b>접수 ID</b></td><td>%s</td></tr>
<tr><td><b>유형</b></td><td>%s</td></tr>
<tr><td><b>이름</b></td><td>%s</td></tr>
<tr><td><b>회사명</b></td><td>%s</td></tr>
<tr><td><b>이메일</b></td><td>%s</td></tr>
<tr><td><b>연락처</b></td><td>%s</td></tr>
</table>
<p><b>문의 내용</b></p>
<pre style="white-space:pre-wrap;font-family:inherit">%s</pre>`,
		html.EscapeString(id), html.EscapeString(typeLabel), html.EscapeString(req.Name),
		html.EscapeString(req.CompanyName), html.EscapeString(req.Email), html.EscapeString(req.Phone),
		html.EscapeString(req.Message))
	if err := s.notify.Send(ctx, to, subject, body); err != nil {
		s.logger.Warn("contact-inquiry: notification email failed", "error", err, "inquiryId", id)
	}
}

// GET /api/admin/inquiries?offset=&limit= — system_admin 전용 목록(최신순).
func (s *Server) handleAdminListContactInquiries(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	offset, limit := parseOffsetLimit(r, 50, 200)
	ctx := r.Context()
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM contact_inquiries`).Scan(&total); err != nil {
		s.logger.Error("admin-inquiries: count failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, COALESCE(company_name,''), email, COALESCE(phone,''), inquiry_type, message,
		       status, user_id, created_at
		FROM contact_inquiries ORDER BY created_at DESC OFFSET $1 LIMIT $2`, offset, limit)
	if err != nil {
		s.logger.Error("admin-inquiries: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name, company, email, phone, typ, message, status string
		var userID sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &company, &email, &phone, &typ, &message, &status, &userID, &createdAt); err != nil {
			s.logger.Error("admin-inquiries: scan failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
			return
		}
		items = append(items, map[string]any{
			"id":               id,
			"name":             name,
			"companyName":      company,
			"email":            email,
			"phone":            phone,
			"inquiryType":      typ,
			"inquiryTypeLabel": contactInquiryTypes[typ],
			"message":          message,
			"status":           status,
			"userId":           nullStringPtr(userID),
			"createdAt":        createdAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "offset": offset, "limit": limit})
}

// PATCH /api/admin/inquiries/{id} {status: new|in_progress|done} — 처리 상태만 바꾼다.
func (s *Server) handleAdminUpdateContactInquiry(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	switch body.Status {
	case "new", "in_progress", "done":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_status"})
		return
	}
	res, err := s.db.ExecContext(r.Context(), `UPDATE contact_inquiries SET status = $1, updated_at = now() WHERE id = $2`, body.Status, r.PathValue("id"))
	if err != nil {
		if strings.Contains(err.Error(), "invalid input syntax") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		s.logger.Error("admin-inquiries: update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": body.Status})
}

// parseOffsetLimit — ?offset=&limit= 공용 파서(기본/상한). 다른 목록 핸들러가 이미 인라인으로
// 처리하고 있어 그대로 두고, 새 핸들러만 이걸 쓴다.
func parseOffsetLimit(r *http.Request, defaultLimit, maxLimit int) (int, int) {
	offset, limit := 0, defaultLimit
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &offset); n != 1 || err != nil || offset < 0 {
			offset = 0
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &limit); n != 1 || err != nil || limit <= 0 {
			limit = defaultLimit
		}
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	return offset, limit
}
