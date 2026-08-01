// Package collector는 이 Go 모듈의 루트 패키지다. go:embed 패턴은 상위
// 디렉터리 참조("..")를 허용하지 않아서, go.mod/CHANGELOG.md와 같은 위치에
// 있는 파일을 내장하려면 그 파일들과 같은 디렉터리에 놓인 소스가 필요하다
// — 그래서 internal/api 같은 하위 패키지 대신 이 파일이 루트에 존재한다.
// 실제 사용처는 internal/api의 관리자(#/admin) 핸들러들.
package collector

import _ "embed"

// ChangelogMD — #/admin의 "업데이트 로그" 섹션이 렌더링하는 원본.
//
//go:embed CHANGELOG.md
var ChangelogMD string

// GoModText — #/admin/tech-spec이 "주요 프레임워크/라이브러리" 목록을
// 뽑아내는 원본(require 블록 중 "// indirect"가 없는 줄만 직접 의존성).
//
//go:embed go.mod
var GoModText string

// AnalyzerRequirementsTxt — analyzer/requirements.txt의 사본(운영 배포엔
// Python이 없어 원본을 런타임에 읽을 수 없다 — analyzer_requirements.txt
// 파일 상단 주석 참고, 원본이 바뀌면 수동으로 함께 갱신해야 한다).
//
//go:embed analyzer_requirements.txt
var AnalyzerRequirementsTxt string
