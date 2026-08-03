// 간편로그인(구글/네이버/카카오) — 표준 OAuth 2.0 Authorization Code 흐름.
// 세션은 auth.go의 signSession/setSessionCookie를 그대로 재사용한다(콜백
// 성공 시 일반 로그인과 동일한 쿠키가 발급되므로, currentUserID를 쓰는
// 나머지 모든 핸들러가 소셜 로그인 여부를 몰라도 자연히 동작한다).
package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"biz-platform/collector/internal/oauth"
)

const (
	oauthStateCookieName = "oauth_state"
	oauthStateTTL        = 10 * time.Minute
)

func generateOAuthState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// oauthCookiePath — 상태 쿠키의 Path를 /api/auth/{provider}로 좁혀둔다.
// 브라우저의 접두사 매칭 규칙상 .../start가 심은 쿠키는 .../callback 요청
// 에도 그대로 실려온다(둘 다 이 접두사로 시작).
func oauthCookiePath(provider string) string {
	return "/api/auth/" + provider
}

// handleOAuthStart — GET /api/auth/{provider}/start. CSRF 방지용 state를
// 생성해 짧은 TTL의 쿠키에 저장하고, 공급자 동의화면으로 302 리다이렉트한다.
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	client, ok := s.oauthProviders[provider]
	if !ok || !client.Configured() {
		http.Error(w, "이 로그인 방식은 아직 설정되지 않았습니다.", http.StatusNotFound)
		return
	}

	state, err := generateOAuthState()
	if err != nil {
		s.logger.Error("oauth-start: state generation failed", "provider", provider, "error", err)
		http.Error(w, "로그인을 시작할 수 없습니다.", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    state,
		Path:     oauthCookiePath(provider),
		Expires:  time.Now().Add(oauthStateTTL),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, client.AuthURL(state), http.StatusFound)
}

// oauthFailureRedirect — 실패 시 로그인 화면으로 돌려보낸다. 프론트는
// ?oauthError=1을 보고 안내 문구를 띄운다(renderLogin 참고).
func (s *Server) oauthFailureRedirect() string {
	return strings.TrimRight(s.appBaseURL, "/") + "/#/auth/login?oauthError=1"
}

// handleOAuthCallback — GET /api/auth/{provider}/callback. state 검증 →
// 코드 교환 → 사용자정보 조회 → resolveOAuthUser로 계정 매칭/생성 →
// 세션 쿠키 발급 → 프론트로 리다이렉트. 모든 실패 경로는 화면에 스택
// 트레이스나 원인 상세를 노출하지 않고 로그인 화면으로 돌려보낸다(서버
// 로그에만 원인을 남긴다) — OAuth 콜백은 사용자가 직접 보는 화면이라
// JSON 에러 응답 대신 항상 302 리다이렉트로 처리한다.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	client, ok := s.oauthProviders[provider]
	if !ok || !client.Configured() {
		http.Error(w, "이 로그인 방식은 아직 설정되지 않았습니다.", http.StatusNotFound)
		return
	}

	// 쓰든 안 쓰든 1회용이므로 검증 여부와 무관하게 바로 만료시킨다.
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookieName, Value: "", Path: oauthCookiePath(provider),
		MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		s.logger.Warn("oauth-callback: provider returned error", "provider", provider, "error", errParam)
		http.Redirect(w, r, s.oauthFailureRedirect(), http.StatusFound)
		return
	}

	stateCookie, err := r.Cookie(oauthStateCookieName)
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		s.logger.Warn("oauth-callback: state mismatch or missing", "provider", provider)
		http.Redirect(w, r, s.oauthFailureRedirect(), http.StatusFound)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		s.logger.Warn("oauth-callback: missing code", "provider", provider)
		http.Redirect(w, r, s.oauthFailureRedirect(), http.StatusFound)
		return
	}

	accessToken, err := client.Exchange(r.Context(), code)
	if err != nil {
		s.logger.Error("oauth-callback: token exchange failed", "provider", provider, "error", err)
		http.Redirect(w, r, s.oauthFailureRedirect(), http.StatusFound)
		return
	}

	email, providerUserID, err := client.FetchUser(r.Context(), accessToken)
	if err != nil || providerUserID == "" {
		s.logger.Error("oauth-callback: fetch user info failed", "provider", provider, "error", err)
		http.Redirect(w, r, s.oauthFailureRedirect(), http.StatusFound)
		return
	}
	email = strings.TrimSpace(strings.ToLower(email))

	userID, err := s.resolveOAuthUser(r.Context(), provider, providerUserID, email)
	if err != nil {
		s.logger.Error("oauth-callback: resolve user failed", "provider", provider, "error", err)
		http.Redirect(w, r, s.oauthFailureRedirect(), http.StatusFound)
		return
	}

	// 관리자가 탈퇴 처리한 계정은 user_oauth_identities 행을 일부러 안
	// 지운다(admin_member_actions.go 주석 참고 — 지우면 resolveOAuthUser가
	// "새 계정"으로 오인해 원래 이메일로 재가입을 허용해버려 탈퇴가
	// 무력화된다). 그래서 여기서 명시적으로 확인해야 실제로 로그인이
	// 막힌다 — 이메일/비밀번호 로그인(handleLogin)처럼 이메일 자체가
	// 바뀌어서 자연히 막히는 부수효과가 없다.
	var deactivatedAt sql.NullTime
	if err := s.db.QueryRowContext(r.Context(), `SELECT deactivated_at FROM users WHERE id = $1`, userID).Scan(&deactivatedAt); err != nil {
		s.logger.Error("oauth-callback: deactivation check failed", "error", err)
		http.Redirect(w, r, s.oauthFailureRedirect(), http.StatusFound)
		return
	}
	if deactivatedAt.Valid {
		s.logger.Warn("oauth-callback: deactivated account attempted login", "provider", provider, "userId", userID)
		http.Redirect(w, r, s.oauthFailureRedirect(), http.StatusFound)
		return
	}

	// 관리자 화면(admin.go) 회원목록의 "마지막 로그인" 근거 — handleLogin과
	// 동일하게 실패해도 로그인 자체를 막지 않는다.
	if _, err := s.db.ExecContext(r.Context(), `UPDATE users SET last_login_at = now() WHERE id = $1`, userID); err != nil {
		s.logger.Error("oauth-callback: last_login_at update failed", "error", err)
	}

	s.setSessionCookie(w, userID)

	// 2026-08-03 온보딩 재설계 이후로는 "온보딩 완료" 기준이 회사 프로필
	// 존재 여부가 아니다(회사 정보 입력은 가입과 완전히 분리됨 — 사업자등록증
	// 온보딩이나 나중에 수동 입력으로 처리). 아직 온보딩을 안 마친 계정은
	// 약관동의(+휴대폰 인증, 켜져있을 때만) 화면(renderOAuthProfileStep,
	// index.html)으로 보낸다. 2026-08-04부터는 phone_verified_at이 아니라
	// terms_agreements 행 존재 여부로 판단한다 — 관리자가 휴대폰 인증을
	// 끌 수 있게 되면서(system_settings.go) phone_verified_at만으로는
	// "온보딩을 마쳤는지"를 알 수 없어졌다(그 계정은 인증 자체를 영원히
	// 안 할 수 있음). 이렇게 안 바꾸면 이미 약관동의까지 마친 재방문
	// 사용자도 로그인할 때마다 매번 온보딩 화면으로 튕기게 된다.
	var onboarded bool
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM terms_agreements WHERE user_id = $1)`, userID,
	).Scan(&onboarded); err != nil {
		s.logger.Error("oauth-callback: onboarded check failed", "error", err)
	}

	target := "/#/"
	if !onboarded {
		target = "/#/auth/oauth-profile"
	}
	http.Redirect(w, r, strings.TrimRight(s.appBaseURL, "/")+target, http.StatusFound)
}

// resolveOAuthUser matches (provider, providerUserID) against
// user_oauth_identities first. If no identity row exists yet, it falls back
// to matching by email against users(기존 이메일 회원가입과 계정 매칭 —
// 스펙 요구사항: "같은 이메일로 이미 가입된 계정이 있으면 그 계정에 연결,
// 없으면 신규 생성"). A brand-new user is created with password_hash=NULL
// (소셜 전용 계정).
func (s *Server) resolveOAuthUser(ctx context.Context, provider, providerUserID, email string) (string, error) {
	var userID string
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM user_oauth_identities WHERE provider = $1 AND provider_user_id = $2`,
		provider, providerUserID,
	).Scan(&userID)
	if err == nil {
		return userID, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("lookup existing identity: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if email != "" {
		err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&userID)
		if err != nil && err != sql.ErrNoRows {
			return "", fmt.Errorf("lookup user by email: %w", err)
		}
	}

	if userID == "" {
		if email == "" {
			return "", fmt.Errorf("provider %s did not return a verified email and no existing identity matched", provider)
		}
		// email_verified_at을 즉시 채운다 — 여기 도달했다는 건 provider가
		// 검증된 이메일을 돌려줬다는 뜻이라(위 "did not return a verified
		// email" 에러 분기 참고), 이메일 가입처럼 별도 인증 메일을 보낼
		// 필요가 없다(email_verification.go 참고).
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO users (email, password_hash, email_verified_at) VALUES ($1, NULL, now()) RETURNING id`,
			email,
		).Scan(&userID); err != nil {
			return "", fmt.Errorf("insert new user: %w", err)
		}
	}

	// ON CONFLICT DO NOTHING — 동시에 같은 계정으로 두 번 콜백이 들어오는
	// 극히 드문 경합에서도 UNIQUE(provider, provider_user_id) 위반으로
	// 500이 나는 대신, 먼저 커밋된 쪽의 결과를 그대로 쓴다.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO user_oauth_identities (user_id, provider, provider_user_id, email)
		 VALUES ($1,$2,$3,$4) ON CONFLICT (provider, provider_user_id) DO NOTHING`,
		userID, provider, providerUserID, sql.NullString{String: email, Valid: email != ""},
	)
	if err != nil {
		return "", fmt.Errorf("insert oauth identity: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT user_id FROM user_oauth_identities WHERE provider = $1 AND provider_user_id = $2`,
			provider, providerUserID,
		).Scan(&userID); err != nil {
			return "", fmt.Errorf("lookup identity after conflict: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return userID, nil
}

// newOAuthProviders builds the provider map from already-constructed
// oauth.Client values (cmd/apiserver/main.go loads env vars and calls
// oauth.NewGoogleClient/NewNaverClient/NewKakaoClient). Providers with no
// client ID configured are still present in the map with Configured()==false
// so handleOAuthStart/handleOAuthCallback can return a clean 404 instead of
// panicking on a missing map key.
func newOAuthProviders(google, naver, kakao oauth.Client) map[string]oauth.Client {
	return map[string]oauth.Client{
		"google": google,
		"naver":  naver,
		"kakao":  kakao,
	}
}
