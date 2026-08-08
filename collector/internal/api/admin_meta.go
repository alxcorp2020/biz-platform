// admin_meta.go — #/admin에 새로 추가하는 3개 섹션(업데이트 로그/기술
// 스펙/외부 연동 현황)의 백엔드. 셋 다 requireSystemAdmin을 공유한다
// (admin.go와 동일한 접근 검사).
//
// 배포가 distroless 단일 Go 바이너리(운영 배포엔 소스 트리 자체가 없음 —
// Dockerfile.apiserver 참고)라 CHANGELOG.md/go.mod/analyzer의
// requirements.txt를 런타임에 os.ReadFile로 읽을 방법이 없다. 그래서 collector
// 모듈 루트(embedded.go)에서 go:embed로 미리 바이너리에 내장해두고 여기서는
// 그 내장된 문자열만 파싱한다 — 기존 webui 정적파일 내장과 같은 방식이다.
package api

import (
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"

	collector "biz-platform/collector"
	"biz-platform/collector/internal/webui"
)

// ---------- 업데이트 로그 (GET /api/admin/changelog) ----------

type changelogSection struct {
	Date  string   `json:"date"`
	Items []string `json:"items"`
}

// parseChangelog — CHANGELOG.md의 "## YYYY-MM-DD" 헤더 + "- " 불릿만 인식하는
// 전용 파서다(범용 마크다운이 아니다 — 이 파일 형식만 우리가 통제하므로
// 프론트에 마크다운 라이브러리를 새로 들이지 않기 위해 서버가 구조화해서
// 내려준다). 파일 상단의 ``` 코드펜스(작성 예시)는 실제 항목이 아니므로
// 건너뛴다.
func parseChangelog(md string) []changelogSection {
	var sections []changelogSection
	var current *changelogSection
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "## "):
			sections = append(sections, changelogSection{Date: strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))})
			current = &sections[len(sections)-1]
		case current != nil && strings.HasPrefix(trimmed, "- "):
			current.Items = append(current.Items, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		}
	}
	return sections
}

func (s *Server) handleAdminChangelog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": parseChangelog(collector.ChangelogMD)})
}

// ---------- 기술 스펙 (GET /api/admin/tech-spec) ----------

type techSpecResponse struct {
	Stack    []techSpecStackItem `json:"stack"`
	Backend  techSpecBackend     `json:"backend"`
	Frontend techSpecFrontend    `json:"frontend"`
	Analyzer techSpecAnalyzer    `json:"analyzer"`
	Database techSpecDatabase    `json:"database"`
	Infra    techSpecInfra       `json:"infra"`
}

// techSpecStackItem — "개발 스택 개요"의 한 항목. 개발자가 나중에 직접 운영할 때
// "무엇을 어떤 버전/상태로 구현했는지"를 한눈에 보고, 상세보기로 구현 내역·핵심
// 파일까지 확인하기 위한 큐레이션 항목이다. 신규 스택이 생기면 techStackItems에
// 항목을 추가한다(코드가 단일 소스 — go.mod/DB 등 실시간 값은 핸들러에서 주입).
type techSpecStackItem struct {
	Category string `json:"category"` // 백엔드/프론트엔드/데이터/AI/알림·결제/인증·보안/인프라
	Name     string `json:"name"`
	Version  string `json:"version"` // 버전 또는 상태(예: "v1.61.0", "구현완료", "운영 설정 필요")
	Summary  string `json:"summary"` // 한 줄 요약
	Detail   string `json:"detail"`  // 상세보기 — 구현 내역/핵심 파일/비고
}

type techSpecBackend struct {
	GoVersion    string   `json:"goVersion"`
	Framework    string   `json:"framework"`
	Dependencies []string `json:"dependencies"`
}

type techSpecFrontend struct {
	Description       string   `json:"description"`
	ExternalResources []string `json:"externalResources"`
}

type techSpecAnalyzer struct {
	PythonVersion string   `json:"pythonVersion"`
	Note          string   `json:"note"`
	Packages      []string `json:"packages"`
}

type techSpecDatabase struct {
	Version string `json:"version"`
}

type techSpecInfra struct {
	Hosting string `json:"hosting"`
	Repo    string `json:"repo"`
}

// parseDirectGoModDeps — go.mod의 require( ) 블록 중 "// indirect" 주석이
// 없는 줄만 뽑는다("주요" 의존성 = 직접 의존성). go.mod는 require 블록이
// 여러 개일 수 있어(직접/간접이 따로 묶임) 블록 경계를 그대로 따라간다.
func parseDirectGoModDeps(goMod string) []string {
	var deps []string
	inRequire := false
	for _, line := range strings.Split(goMod, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "require (":
			inRequire = true
		case inRequire && trimmed == ")":
			inRequire = false
		case inRequire && trimmed != "" && !strings.Contains(trimmed, "// indirect"):
			deps = append(deps, trimmed)
		}
	}
	return deps
}

// parseRequirementsTxt — requirements.txt 형식에서 순수 패키지==버전 줄만
// 뽑는다(주석 줄/빈 줄 제외, 줄 끝 "# 설명" 주석도 제거).
func parseRequirementsTxt(txt string) []string {
	var pkgs []string
	for _, line := range strings.Split(txt, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if idx := strings.Index(trimmed, "#"); idx >= 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
		if trimmed != "" {
			pkgs = append(pkgs, trimmed)
		}
	}
	return pkgs
}

// scriptSrcRe/linkHrefRe — <script src="http...">와 <link href="http...">
// 태그만 잡는다. 본문 어딘가의 일반 <a href="http...">(예: 관리자 대시보드가
// 안내하는 platform.claude.com 링크)까지 걸리면 "프론트엔드가 로드하는
// 외부 리소스"가 아닌 걸 리소스로 오인하게 되므로 태그 종류를 좁혀 둔다.
var (
	scriptSrcRe = regexp.MustCompile(`<script[^>]*\ssrc="(https?://[^"]+)"`)
	linkHrefRe  = regexp.MustCompile(`<link[^>]*\shref="(https?://[^"]+)"`)
)

// extractExternalResources — 내장된 index.html에서 실제로 쓰는 외부
// CDN 스크립트/스타일시트 URL을 그대로 스캔한다(하드코딩 시 index.html이
// 바뀌어도 안 맞을 수 있어 원본에서 직접 추출).
func extractExternalResources(indexHTML []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, re := range []*regexp.Regexp{scriptSrcRe, linkHrefRe} {
		for _, m := range re.FindAllSubmatch(indexHTML, -1) {
			url := string(m[1])
			if !seen[url] {
				seen[url] = true
				out = append(out, url)
			}
		}
	}
	return out
}

// techStackItems — 플랫폼 개발 스택 개요(큐레이션). goVersion/dbVersion 등
// 실시간 값만 인자로 주입하고 나머지는 정적으로 관리한다. 신규 스택이 추가되면
// 이 목록에 항목을 더한다.
func techStackItems(goVersion, dbVersion string) []techSpecStackItem {
	return []techSpecStackItem{
		{
			Category: "언어·런타임", Name: "Go (apiserver + collector)", Version: goVersion,
			Summary: "표준 net/http 단일 바이너리 백엔드 — 웹 프레임워크 없음",
			Detail: "API 서버(cmd/apiserver)와 공고 수집기(cmd/collector)를 하나의 Go 모듈로 빌드한다. " +
				"라우팅은 Go 1.22 표준 ServeMux(패턴 매칭)만 사용하고, 프론트엔드(index.html)는 go:embed로 " +
				"바이너리에 내장해 별도 배포물이 없다. apiserver 기동 시 migrate.Apply가 스키마 마이그레이션을 " +
				"자동 적용한다(ADD COLUMN IF NOT EXISTS 멱등 패턴).",
		},
		{
			Category: "언어·런타임", Name: "Python (analyzer)", Version: "3.11 (로컬 실행)",
			Summary: "문서 분석 보조 파이프라인 — Render 배포에는 미포함",
			Detail: "analyzer는 로컬 cron/launchd로 별도 실행되는 프로세스로, 운영 Web Service(distroless 이미지)에는 " +
				"포함되지 않는다(analyzer/run_pipeline.sh). 의존 패키지는 아래 '분석 엔진' 카드의 requirements.txt 참고.",
		},
		{
			Category: "프론트엔드", Name: "Vanilla JS SPA", Version: "빌드 과정 없음",
			Summary: "단일 index.html — 프레임워크·번들러 없이 해시 라우팅",
			Detail: "React/Vue 등 프레임워크 없이 순수 JS로 작성한 단일 파일 SPA다. route()가 location.hash로 화면을 " +
				"전환하고, 모달/토스트/드롭다운/다크모드 토글 등 공용 컴포넌트도 직접 구현했다. go:embed로 서버 바이너리에 " +
				"내장되어 CDN 폰트·토스 결제 위젯 정도만 외부 리소스로 로드한다.",
		},
		{
			Category: "데이터베이스", Name: "PostgreSQL", Version: dbVersion,
			Summary: "자체 마이그레이션 · 세션 테이블 없음(서명 쿠키)",
			Detail: "스키마는 코드(internal/migrate/migrate.go)로 관리하며 기동 시 순차 적용한다. 세션은 별도 테이블 없이 " +
				"HMAC 서명 쿠키로 처리한다. 공고는 notices(현재값) + notice_versions(버전 이력, 원문 raw_documents 링크) " +
				"구조이고, 변경 감지(changedetect)로 정정공고를 필드 단위로 추적한다.",
		},
		{
			Category: "데이터 수집", Name: "나라장터 G2B (입찰공고)", Version: "구현완료",
			Summary: "조달청 용역 입찰공고 수집 — 분류 코드·계층·업종제한 캡처",
			Detail: "용역 부문 입찰공고를 주기 수집한다. 공공조달분류(대/중/세분류명 + 8자리 코드)와 지역제한·업종제한 여부, " +
				"취소공고(ntceKindNm) 감지까지 저장한다(2026-08-08 Phase 0에서 분류 코드·계층·업종제한 플래그 추가). " +
				"인증키 G2B_SERVICE_KEY 하나를 낙찰정보서비스와 공유한다.",
		},
		{
			Category: "데이터 수집", Name: "기업마당 bizinfo / 낙찰정보 ScsbidInfo", Version: "부분 운영",
			Summary: "지원사업 공고 + 동일 발주기관 낙찰이력",
			Detail: "bizinfo(중앙부처·지자체 지원사업)와 ScsbidInfoService(낙찰이력, 경쟁사 분석용)를 같은 수집 파이프라인" +
				"(runner)으로 처리한다. 원문은 raw_documents에 그대로 보관해 필요 시 재파싱(백필)에 쓴다. 각 소스 키 설정 " +
				"여부는 관리자 '외부 연동 현황'에서 실시간 확인한다.",
		},
		{
			Category: "AI", Name: "Anthropic Claude", Version: "sdk v1.61.0",
			Summary: "사업자등록증·면허·재무 추출 / 공고 요약 / 참여판단",
			Detail: "anthropic-sdk-go로 Claude를 호출한다. 사업자등록증·면허증·재무제표 등 문서를 image/document 블록으로 " +
				"넣고 tool use(strict JSON 스키마)로 구조화 추출한다. 공고 요약과 참여판단 등급 산정에도 사용한다. " +
				"ANTHROPIC_API_KEY 필요(미설정 시 관련 기능만 비활성).",
		},
		{
			Category: "결제·알림", Name: "Toss Payments", Version: "구현완료",
			Summary: "구독 결제/환불 — 서버 승인 재확인",
			Detail: "구독 결제는 토스 결제위젯으로 시작하고, 서버가 승인(confirm)을 재확인해 금액 위변조를 막는다. 환불은 " +
				"전액환불/해지 정책과 하위 구독 복귀(bestValidPriorPayment)까지 처리한다(billing/). TOSS_SECRET_KEY/CLIENT_KEY 필요.",
		},
		{
			Category: "결제·알림", Name: "Resend · Aligo · Web Push(VAPID)", Version: "이메일 운영 / SMS·푸시 설정 필요",
			Summary: "이메일(Resend) · SMS(Aligo) · 웹푸시(VAPID) 3채널",
			Detail: "알림은 담당자 단위로 이메일(Resend)/SMS(Aligo)/웹푸시(VAPID) 채널을 개별 설정한다. SMS는 유료 플랜 전용" +
				"(발송 시점 게이트). 각 채널은 키 미설정 시 조용히 스킵하고 다른 채널은 정상 동작한다(notify/, push_notifications.go).",
		},
		{
			Category: "인증·보안", Name: "세션 쿠키(HMAC) · 소셜로그인 OAuth", Version: "구현완료",
			Summary: "HMAC-SHA256 서명 세션 + 구글/네이버/카카오 로그인",
			Detail: "세션은 서버 저장 없이 user_id+만료를 HMAC-SHA256(SESSION_SECRET)로 서명한 쿠키로 관리한다(auth.go). " +
				"비밀번호는 bcrypt. 간편로그인은 Google/Naver/Kakao OAuth 2.0(oauth/)이며, 각 제공자 콘솔에 redirect_uri " +
				"등록과 APP_BASE_URL 설정이 필요하다.",
		},
		{
			Category: "인프라", Name: "오브젝트 스토리지(S3 호환) · Render 배포", Version: "구현완료",
			Summary: "MinIO(로컬)/R2·S3(운영) · Render Docker 배포",
			Detail: "업로드 문서/첨부는 S3 호환 스토리지에 저장한다(로컬 MinIO, 운영 Cloudflare R2 또는 AWS S3 — STORAGE_* env). " +
				"배포는 Render Web Service(Docker, render.yaml + Dockerfile.apiserver/collector), 소스는 GitHub. " +
				"VAPID/OAuth 등 런타임 키는 Render 대시보드 환경변수로 주입한다.",
		},
	}
}

func (s *Server) handleAdminTechSpec(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	indexHTML, err := webui.IndexHTML()
	if err != nil {
		s.logger.Error("admin-tech-spec: index.html read failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read_failed"})
		return
	}
	var dbVersion string
	if err := s.db.QueryRowContext(r.Context(), `SELECT version()`).Scan(&dbVersion); err != nil {
		s.logger.Error("admin-tech-spec: db version query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	writeJSON(w, http.StatusOK, techSpecResponse{
		Stack: techStackItems(runtime.Version(), dbVersion),
		Backend: techSpecBackend{
			GoVersion:    runtime.Version(),
			Framework:    "표준 라이브러리 net/http (별도 웹 프레임워크 없음)",
			Dependencies: parseDirectGoModDeps(collector.GoModText),
		},
		Frontend: techSpecFrontend{
			Description:       "정적 HTML/JS 단일 파일(index.html) — 프론트엔드 프레임워크·빌드 과정 없음",
			ExternalResources: extractExternalResources(indexHTML),
		},
		Analyzer: techSpecAnalyzer{
			PythonVersion: "3.11 (로컬 개발 환경 기준 — 버전이 컨테이너로 고정되어 있지 않음)",
			Note:          "analyzer는 Render 운영 배포(distroless 이미지)에 포함되지 않고, 로컬 cron/launchd로 별도 실행되는 프로세스다(analyzer/run_pipeline.sh 참고).",
			Packages:      parseRequirementsTxt(collector.AnalyzerRequirementsTxt),
		},
		Database: techSpecDatabase{Version: dbVersion},
		Infra: techSpecInfra{
			Hosting: "Render (Web Service, Docker 런타임 — render.yaml 기준)",
			Repo:    "GitHub",
		},
	})
}

// ---------- 외부 연동/API 현황 (GET /api/admin/integrations) ----------

type adminIntegrationItem struct {
	Name       string   `json:"name"`
	Purpose    string   `json:"purpose"`
	EnvVars    []string `json:"envVars"`
	DocURL     string   `json:"docUrl"`
	Configured bool     `json:"configured"`
}

// adminIntegrationDefs — 하드코딩된 목록(사용자 요청대로 이번엔 정적
// 목록으로 시작). 실제 API 호출 성공 여부는 확인하지 않고, 필요한
// 환경변수가 전부 채워져 있는지만("설정됨/설정안됨") 실시간으로 본다.
var adminIntegrationDefs = []struct {
	Name    string
	Purpose string
	EnvVars []string
	DocURL  string
}{
	{
		Name:    "나라장터 입찰공고정보서비스 (G2B)",
		Purpose: "조달청 용역 부문 입찰공고 수집",
		EnvVars: []string{"G2B_SERVICE_KEY"},
		DocURL:  "https://www.data.go.kr",
	},
	{
		Name:    "나라장터 낙찰정보서비스 (ScsbidInfoService)",
		Purpose: "동일 발주기관 낙찰이력(경쟁사 분석) 수집 — G2B_SERVICE_KEY를 그대로 재사용",
		EnvVars: []string{"G2B_SERVICE_KEY"},
		DocURL:  "https://www.data.go.kr/data/15129397/openapi.do",
	},
	{
		Name:    "기업마당 지원사업 Open API",
		Purpose: "중앙부처·지자체 지원사업 공고 수집",
		EnvVars: []string{"BIZINFO_API_KEY"},
		DocURL:  "https://www.bizinfo.go.kr/apiDetail.do?id=bizinfoApi",
	},
	{
		Name:    "Anthropic API (Claude)",
		Purpose: "서류 AI 분석/요약/참여판단 등급 산정",
		EnvVars: []string{"ANTHROPIC_API_KEY"},
		DocURL:  "https://docs.claude.com",
	},
	{
		Name:    "Resend",
		Purpose: "이메일 알림 발송",
		EnvVars: []string{"RESEND_API_KEY", "RESEND_FROM_EMAIL"},
		DocURL:  "https://resend.com/docs",
	},
	{
		Name:    "알리고 (Aligo)",
		Purpose: "SMS/알림톡 발송",
		EnvVars: []string{"ALIGO_API_KEY", "ALIGO_USER_ID", "ALIGO_SENDER"},
		DocURL:  "https://smartsms.aligo.in",
	},
	{
		Name:    "토스페이먼츠 (Toss Payments)",
		Purpose: "구독 결제 (API 개별연동 방식)",
		EnvVars: []string{"TOSS_CLIENT_KEY", "TOSS_SECRET_KEY"},
		DocURL:  "https://docs.tosspayments.com",
	},
}

func (s *Server) handleAdminIntegrations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}
	items := make([]adminIntegrationItem, 0, len(adminIntegrationDefs))
	for _, d := range adminIntegrationDefs {
		configured := true
		for _, ev := range d.EnvVars {
			if os.Getenv(ev) == "" {
				configured = false
				break
			}
		}
		items = append(items, adminIntegrationItem{
			Name:       d.Name,
			Purpose:    d.Purpose,
			EnvVars:    d.EnvVars,
			DocURL:     d.DocURL,
			Configured: configured,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
