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
	Backend  techSpecBackend  `json:"backend"`
	Frontend techSpecFrontend `json:"frontend"`
	Analyzer techSpecAnalyzer `json:"analyzer"`
	Database techSpecDatabase `json:"database"`
	Infra    techSpecInfra    `json:"infra"`
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
