// company_team.go — 팀기능: 한 조직(company_profiles)에 최대 3명(Business
// 플랜)/1명(Free·Basic·Pro, 본인뿐)까지 소속. 권한 2단계 — owner(초대·
// 구성원 관리, 구독 관리, 그 외 전체 데이터 쓰기), member(파이프라인
// 조회+참여만, 그 외는 읽기 전용). 초대는 이메일 링크(Resend 재사용) +
// 무작위 토큰이고, 받은 사람이 그 링크로 가입/로그인하면 자동 합류한다.
//
// 한 로그인 계정은 항상 조직 하나에만 속한다(company_members.user_id
// UNIQUE) — 여러 회사에 동시 소속되는 시나리오는 이번 범위 밖이라, 이미
// 다른 조직에 속한 사용자는 초대를 받아도 합류할 수 없다(명확한 에러로
// 안내).
package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"

	"biz-platform/collector/internal/billing"
)

const invitationValidity = 7 * 24 * time.Hour

// Phase 5 2단계: 팀 초대 발송/수락 이벤트도 notification_log를 거치게 해
// 다른 채널(이메일 리마인더/다이제스트 등)과 동일하게 발송 이력·실패
// 사유가 남게 한다 — 이전엔 s.notify.Send를 직접 호출해 실패해도 서버
// 로그에만 남고 감사 이력이 전혀 없었다.
const (
	notifyEventTeamInvite         = "team_invite"
	notifyEventTeamInviteAccepted = "team_invite_accepted"
)

// maxTeamMembers returns how many company_members rows (owner 포함) an org
// on this effective plan may have — Business만 팀(최대 3명), 나머지는
// 본인 1명뿐이라는 스펙을 그대로 상수화.
func maxTeamMembers(plan billing.Plan) int {
	if plan == billing.PlanBusiness {
		return 3
	}
	return 1
}

func generateInvitationToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type companyMemberItem struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

// handleListCompanyMembers — owner/member 둘 다 조회 가능(팀원 명단 자체는
// "전체데이터 쓰기"가 아니라 조직에 속한 사람이면 당연히 볼 수 있는
// 정보로 취급).
func (s *Server) handleListCompanyMembers(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("list-members: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []companyMemberItem{}})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT cm.id, cm.user_id, u.email, cm.role, cm.created_at
		FROM company_members cm
		JOIN users u ON u.id = cm.user_id
		WHERE cm.company_profile_id = $1
		ORDER BY (cm.role = 'owner') DESC, cm.created_at`, profile.ID)
	if err != nil {
		s.logger.Error("list-members: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer rows.Close()

	items := []companyMemberItem{}
	for rows.Next() {
		var it companyMemberItem
		if err := rows.Scan(&it.ID, &it.UserID, &it.Email, &it.Role, &it.CreatedAt); err != nil {
			continue
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleRemoveCompanyMember — owner-only. owner 자신은 이 엔드포인트로
// 제거할 수 없다(소유권 이전 기능이 이번 범위에 없어, owner가 없어지는
// 상태를 아예 만들지 않는다).
func (s *Server) handleRemoveCompanyMember(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("remove-member: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return
	}

	memberID := r.PathValue("id")
	var targetRole string
	err = s.db.QueryRowContext(r.Context(),
		`SELECT role FROM company_members WHERE id = $1 AND company_profile_id = $2`, memberID, profile.ID,
	).Scan(&targetRole)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member_not_found"})
		return
	}
	if targetRole == "owner" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot_remove_owner"})
		return
	}

	if _, err := s.db.ExecContext(r.Context(),
		`DELETE FROM company_members WHERE id = $1 AND company_profile_id = $2`, memberID, profile.ID,
	); err != nil {
		s.logger.Error("remove-member: delete failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

type invitationRequest struct {
	Email string `json:"email"`
}

// handleCreateInvitation — owner-only. 현재 인원(수락된 멤버 수 + 만료
// 안 된 대기 중 초대 수)이 플랜 한도에 도달했으면 거부한다 — 초대를 마구
// 보내놓고 전부 수락되면 한도를 넘기는 것을 막기 위해 "대기 중" 초대도
// 자리 하나로 센다.
func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("create-invitation: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}
	if profile.Role != "owner" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "owner_only"})
		return
	}

	var req invitationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_email"})
		return
	}

	plan, err := s.effectivePlan(r.Context(), profile.ID)
	if err != nil {
		s.logger.Error("create-invitation: effective plan lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	limit := maxTeamMembers(plan)

	var occupiedSeats int
	if err := s.db.QueryRowContext(r.Context(), `
		SELECT
			(SELECT COUNT(*) FROM company_members WHERE company_profile_id = $1) +
			(SELECT COUNT(*) FROM company_invitations
			 WHERE company_profile_id = $1 AND status = 'pending' AND expires_at > now())
	`, profile.ID).Scan(&occupiedSeats); err != nil {
		s.logger.Error("create-invitation: seat count failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if occupiedSeats >= limit {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "team_size_limit_exceeded", "limit": limit})
		return
	}

	var alreadyMember bool
	if err := s.db.QueryRowContext(r.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM company_members cm JOIN users u ON u.id = cm.user_id
			WHERE cm.company_profile_id = $1 AND lower(u.email) = $2
		)`, profile.ID, email).Scan(&alreadyMember); err != nil {
		s.logger.Error("create-invitation: member check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if alreadyMember {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "already_member"})
		return
	}

	token, err := generateInvitationToken()
	if err != nil {
		s.logger.Error("create-invitation: token generation failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	var invitationID string
	if err := s.db.QueryRowContext(r.Context(), `
		INSERT INTO company_invitations (company_profile_id, email, token, invited_by_user_id, expires_at)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		profile.ID, email, token, userID, time.Now().Add(invitationValidity),
	).Scan(&invitationID); err != nil {
		s.logger.Error("create-invitation: insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	if s.notify != nil {
		var inviterEmail string
		_ = s.db.QueryRowContext(r.Context(), `SELECT email FROM users WHERE id = $1`, userID).Scan(&inviterEmail)
		inviteLink := s.appBaseURL + "/#/invite?token=" + token
		subject := "팀 초대: 참여판단 AI 비서"
		body := fmt.Sprintf(
			"<p><b>%s</b>님이 회사 팀에 초대했습니다.</p><p>아래 링크로 접속해 가입 또는 로그인하면 자동으로 합류됩니다.</p><p><a href=\"%s\">%s</a></p><p>이 링크는 7일간 유효합니다.</p>",
			html.EscapeString(inviterEmail), inviteLink, inviteLink,
		)
		sendErr := s.notify.Send(r.Context(), email, subject, body)
		status, errMsg := "sent", sql.NullString{}
		if sendErr != nil {
			status = "failed"
			errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
			s.logger.Error("create-invitation: send failed", "error", sendErr)
		}
		if _, logErr := s.db.ExecContext(r.Context(), `
			INSERT INTO notification_log (event_type, channel, recipient_email, subject, status, error_message)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			notifyEventTeamInvite, notifyChannelEmail, email, subject, status, errMsg,
		); logErr != nil {
			s.logger.Error("create-invitation: log insert failed", "error", logErr)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": invitationID, "email": email})
}

type invitationDTO struct {
	CompanyRegion   *string  `json:"companyRegion"`
	CompanyIndustry []string `json:"companyIndustry"`
	InviterEmail    string   `json:"inviterEmail"`
	Email           string   `json:"email"`
	Status          string   `json:"status"`
	Expired         bool     `json:"expired"`
}

// handleGetInvitation — 공개 엔드포인트(로그인 불필요). 초대 링크를 클릭한
// 사람이 로그인/가입 전에 "누가 어떤 회사로 초대했는지" 보여주기 위함.
func (s *Server) handleGetInvitation(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	var dto invitationDTO
	var region sql.NullString
	var industry pq.StringArray
	var status string
	var expiresAt time.Time
	err := s.db.QueryRowContext(r.Context(), `
		SELECT cp.region, cp.industry, u.email, ci.email, ci.status, ci.expires_at
		FROM company_invitations ci
		JOIN company_profiles cp ON cp.id = ci.company_profile_id
		JOIN users u ON u.id = ci.invited_by_user_id
		WHERE ci.token = $1`, token,
	).Scan(&region, &industry, &dto.InviterEmail, &dto.Email, &status, &expiresAt)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "invitation_not_found"})
		return
	}
	dto.CompanyRegion = nullStringPtr(region)
	dto.CompanyIndustry = []string(industry)
	dto.Status = status
	dto.Expired = status == "pending" && time.Now().After(expiresAt)
	writeJSON(w, http.StatusOK, dto)
}

// handleAcceptInvitation requires the caller to already be logged in (with
// an account whose email case-insensitively matches the invited address —
// 아무나 링크를 주워서 합류하는 것을 막는 최소한의 안전장치). 이미 다른
// 조직에 속해 있으면 거부(한 계정은 조직 하나에만 소속).
func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	token := r.PathValue("token")

	var invitationID, companyProfileID, invitedEmail, status, invitedByUserID string
	var expiresAt time.Time
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, company_profile_id, email, status, expires_at, invited_by_user_id FROM company_invitations WHERE token = $1`, token,
	).Scan(&invitationID, &companyProfileID, &invitedEmail, &status, &expiresAt, &invitedByUserID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "invitation_not_found"})
		return
	}
	if status != "pending" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invitation_not_pending"})
		return
	}
	if time.Now().After(expiresAt) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invitation_expired"})
		return
	}

	var callerEmail string
	if err := s.db.QueryRowContext(r.Context(), `SELECT email FROM users WHERE id = $1`, userID).Scan(&callerEmail); err != nil {
		s.logger.Error("accept-invitation: caller lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if !strings.EqualFold(callerEmail, invitedEmail) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "email_mismatch"})
		return
	}

	var alreadyInOrg bool
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM company_members WHERE user_id = $1)`, userID,
	).Scan(&alreadyInOrg); err != nil {
		s.logger.Error("accept-invitation: membership check failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if alreadyInOrg {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "already_in_organization"})
		return
	}

	// 그 사이 다른 초대가 먼저 수락돼 자리가 다 찼을 수 있으니 여기서
	// 한 번 더 확인한다(생성 시점 체크는 TOCTOU 창을 완전히 막지 못함).
	plan, err := s.effectivePlan(r.Context(), companyProfileID)
	if err != nil {
		s.logger.Error("accept-invitation: effective plan lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	var memberCount int
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM company_members WHERE company_profile_id = $1`, companyProfileID,
	).Scan(&memberCount); err != nil {
		s.logger.Error("accept-invitation: member count failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if memberCount >= maxTeamMembers(plan) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "team_size_limit_exceeded"})
		return
	}
	profileID := companyProfileID

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.logger.Error("accept-invitation: begin tx failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO company_members (company_profile_id, user_id, role) VALUES ($1,$2,'member')`,
		profileID, userID,
	); err != nil {
		s.logger.Error("accept-invitation: member insert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE company_invitations SET status = 'accepted', accepted_at = now() WHERE id = $1`, invitationID,
	); err != nil {
		s.logger.Error("accept-invitation: invitation update failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("accept-invitation: commit failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	// Phase 5 2단계: 팀원이 초대를 수락해도 초대한 오너에게 알림이 전혀
	// 없었다 — 인앱 알림함(항상 남김, 다른 이벤트와 동일 원칙)과 이메일
	// (설정돼 있을 때만) 양쪽으로 알린다.
	notifySubject := fmt.Sprintf("%s님이 팀에 합류했습니다", callerEmail)
	if err := s.insertInAppNotification(r.Context(), nil, &invitedByUserID, notifyEventTeamInviteAccepted,
		notifySubject, "초대를 수락해 조직에 합류했습니다.", nil, nil); err != nil {
		s.logger.Error("accept-invitation: in-app notification insert failed", "error", err)
	}
	if s.notify != nil {
		var inviterEmail string
		if err := s.db.QueryRowContext(r.Context(), `SELECT email FROM users WHERE id = $1`, invitedByUserID).Scan(&inviterEmail); err != nil {
			s.logger.Error("accept-invitation: inviter lookup failed", "error", err)
		} else {
			body := fmt.Sprintf("<p><b>%s</b>님이 초대를 수락해 조직에 합류했습니다.</p>", html.EscapeString(callerEmail))
			sendErr := s.notify.Send(r.Context(), inviterEmail, notifySubject, body)
			sendStatus, errMsg := "sent", sql.NullString{}
			if sendErr != nil {
				sendStatus = "failed"
				errMsg = sql.NullString{String: sendErr.Error(), Valid: true}
				s.logger.Error("accept-invitation: notify inviter failed", "error", sendErr)
			}
			if _, logErr := s.db.ExecContext(r.Context(), `
				INSERT INTO notification_log (event_type, channel, recipient_email, user_id, subject, status, error_message)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				notifyEventTeamInviteAccepted, notifyChannelEmail, inviterEmail, invitedByUserID, notifySubject, sendStatus, errMsg,
			); logErr != nil {
				s.logger.Error("accept-invitation: log insert failed", "error", logErr)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "joined"})
}
