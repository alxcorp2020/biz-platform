// notice_share.go — 원클릭 참여검토(Phase 1) 상단 버튼 중 "담당자에게
// 전달". 별도 메신저 없이 공고 링크를 이메일 한 통으로 동료에게 보낸다
// (기획서 "핵심 문제": "팀원에게 공고 전달"을 수동으로 하지 않게 함).
// 기존 s.notify(Resend) 클라이언트를 그대로 재사용하고, 발송 성공/실패와
// 무관하게 활동 이력을 남긴다(실패해도 "시도했다"는 사실 자체가 감사
// 대상).
package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
)

func (s *Server) handleShareNotice(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	noticeID := r.PathValue("id")
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("share-notice: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	recipient := strings.TrimSpace(req.Email)
	if recipient == "" || !strings.Contains(recipient, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_email"})
		return
	}

	var title string
	var org sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT title, organization_name FROM notices WHERE id = $1`, noticeID).Scan(&title, &org)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "notice_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("share-notice: notice lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	if s.notify == nil || !s.notify.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "email_not_configured"})
		return
	}

	var senderEmail string
	if err := s.db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&senderEmail); err != nil {
		s.logger.Error("share-notice: sender lookup failed", "error", err)
	}
	link := s.appBaseURL + "/#/notices/" + noticeID
	subject := fmt.Sprintf("[공고 공유] %s", title)
	body := fmt.Sprintf(
		"<p>%s님이 검토를 요청한 공고입니다.</p><p><b>%s</b></p><p>발주기관: %s</p><p><a href=\"%s\">공고 상세 보기 &rarr;</a></p>",
		html.EscapeString(senderEmail), html.EscapeString(title), html.EscapeString(org.String), html.EscapeString(link),
	)

	sendErr := s.notify.Send(ctx, recipient, subject, body)
	s.recordAuditLog(ctx, userID, "notice_shared", "notice", noticeID, map[string]any{
		"toEmail": recipient, "success": sendErr == nil,
	})
	if sendErr != nil {
		s.logger.Error("share-notice: send failed", "recipient", recipient, "error", sendErr)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "send_failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
