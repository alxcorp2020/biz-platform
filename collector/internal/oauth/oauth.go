// Package oauth implements the OAuth 2.0 Authorization Code flow for the
// 간편로그인(구글/네이버/카카오) 소셜 로그인 provider들. 각 provider는 토큰
// 교환/사용자정보 조회 응답 형식이 서로 달라(네이버는 GET 토큰 엔드포인트,
// 카카오는 client_secret이 선택 등) 공통 로직을 억지로 추상화하지 않고
// provider별 파일(google.go/naver.go/kakao.go)에 각자 구현했다 — 공유하는
// 건 이 인터페이스와 HTTP 클라이언트 타임아웃 관례(notify/client.go,
// billing/toss.go와 동일하게 10초)뿐이다.
package oauth

import "context"

// Client is satisfied by each provider's concrete type
// (*GoogleClient/*NaverClient/*KakaoClient). api.Server keeps a
// map[string]Client keyed by provider name ("google"/"naver"/"kakao") so
// oauth_login.go's handlers can dispatch on r.PathValue("provider") without
// a switch per provider.
type Client interface {
	// Configured reports whether this provider's client ID(/secret)이
	// 설정됐는지. 키가 없으면 handleOAuthStart/handleOAuthCallback 둘 다
	// 404를 돌려준다(다른 외부 서비스 클라이언트들과 동일한 "미설정 시
	// 조용히 비활성화" 관례).
	Configured() bool
	// AuthURL builds the provider's 동의화면 URL. state는 CSRF 방지용
	// 1회성 토큰(handleOAuthStart가 생성해 쿠키에도 같이 저장).
	AuthURL(state string) string
	// Exchange trades an authorization code for an access token.
	Exchange(ctx context.Context, code string) (accessToken string, err error)
	// FetchUser resolves the access token to (email, provider 쪽 고유
	// 사용자 ID). email이 빈 문자열일 수 있다(예: 카카오는 이메일 동의를
	// 안 받은 경우) — 호출부(resolveOAuthUser)가 이 경우를 에러로 처리한다.
	FetchUser(ctx context.Context, accessToken string) (email, providerUserID string, err error)
}
