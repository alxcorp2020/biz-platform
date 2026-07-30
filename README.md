# 공공사업 참여판단 데이터 플랫폼

스펙 문서 20~23단계 기준 1단계(기반 설계) + 2단계(Go 수집엔진) + 3단계(버전·변경감지)
핵심 골격 구현. **로컬에서 전체 파이프라인(수집 데몬 → Postgres → API 서버)을 실제로
기동해 검증 완료.**

## 지금까지 구현된 것

- **DB 스키마** (`db/migrations/001_init.sql`): 데이터 출처, 수집작업, 원본문서,
  공고, 공고버전, 첨부파일, 변경이력, 자격조건, 기업프로필, 적합성판정,
  분석크레딧, 감사로그 — 스펙 5장 데이터 모델 전체 반영
- **Go 수집엔진** (`collector/`):
  - `internal/collector/interface.go` — 출처 무관 공통 수집 인터페이스 (6.1)
  - `internal/collector/common/` — 호출 제한(6.3), 재시도(6.4), 해시(6.5)
  - `internal/collector/changedetect/` — 구조화 필드 변경 감지·중요도 분류 (6.6)
  - `internal/collector/store/` — 저장 계층 인터페이스 + 개발용 인메모리 구현
  - `internal/collector/pgstore/` — **저장 계층의 실제 Postgres 구현체**
  - `internal/migrate/` — 앱 기동 시 스키마 자동 적용 (별도 마이그레이션 도구 불필요)
  - `internal/collector/sources/sample/` — 표준 JSON API 출처 샘플 구현체
  - `internal/collector/sources/demo/` — **실제 기관 API 연동 전, 배포 후 바로
    확인 가능하도록 하는 데모 데이터 소스** (HTTP 호출 없음, 4건의 예시 공고)
  - `internal/collector/runner/` — 오케스트레이터
  - `internal/api/` — **공개 REST API** (`GET /api/notices`, `GET /api/notices/{id}`, `GET /healthz`)
  - `cmd/collector/main.go` — 로컬 데모(가짜 API 서버, DB 불필요)
  - `cmd/collector-daemon/main.go` — **배포용 수집 데몬** (1시간 주기 반복 수집)
  - `cmd/apiserver/main.go` — **배포용 API 서버** ($PORT, $DATABASE_URL 사용)

## 아직 구현되지 않은 것 (다음 순서)

- 실제 데이터 출처 연동 (나라장터 등 — API 신청/키 발급 필요, 지금은 데모 데이터로 동작)
- 첨부파일 다운로드 + 오브젝트 스토리지 업로드
- Python 문서 분석 (4단계)
- 규칙 엔진 (5단계)
- 프론트엔드 (6단계) — 지금은 JSON API만 존재

## 미결 과제 (Known Limitations)

- **HWP/HWPX 표 내용 추출 미지원**: `pyhwp`(HWP)/`hwp-hwpx-parser`(HWPX)가 표 셀
  내용을 뽑지 못함. HWP는 `<표>` 플레이스홀더로 남지만(102건 중 98건), **HWPX는
  마커조차 남기지 않고 표 내용이 그대로 사라짐**(71건 중 0건에 마커) — 실제로는
  HWPX 쪽이 더 심각하다. 첨부파일 253건 중 131건, 약 52%에 영향. 현재는 화면에
  "규칙 기반" 배지 + 경고 툴팁으로 한계를 투명하게 표시하고, AI 보완(2차,
  `analyzer/ai_extract.py`)으로 일부 완화 중.
- **LibreOffice headless 변환 방향 조사 결과 — 폐기**: LibreOffice 26.2.5로 실제
  첨부파일 20건(HWP 10 + HWPX 10) 표본을 `soffice --headless --convert-to pdf`로
  변환 테스트. HWP 성공률 10%(1/10, 그마저 표 없는 문서라 표 추출 검증은 못 함),
  HWPX 성공률 0%(0/10) — 나머지는 전부 `Error: source file could not be loaded`.
  표준 LibreOffice의 HWP/HWPX 임포트 필터가 실제 나라장터 문서를 거의 열지
  못해서 이 방향은 폐기한다.
- **다음으로 검토할 대안**: 한글과컴퓨터 공식 API/SDK(유료, 라이선스 필요) 또는
  상용 변환 API(외부로 입찰문서 전송 필요 — 민감정보 우려). 다만 AI 보완(2차)이
  이미 review_required 항목을 상당 부분 커버하고 있어, 변환 경로를 새로 뚫기보다
  AI 보완 커버리지를 넓히는 쪽이 더 현실적일 수 있다.
- **우선순위**: 전체 기능 완성 후 재검토 예정.

## 로컬 실행

### 1. 인프라 (Postgres + MinIO)

```bash
cp .env.example .env
docker compose up -d
```

### 2-A. 빠른 데모 (DB 없이, 가짜 API 서버 사용)

```bash
cd collector
go run ./cmd/collector
```

### 2-B. 실제 배포와 동일한 구성 (Postgres 필요)

```bash
export DATABASE_URL="postgres://biz:bizpassword@localhost:5432/biz_platform?sslmode=disable"

# 최초 실행 시 스키마 자동 생성 + 데모 공고 4건 수집
go run ./cmd/collector-daemon &

# API 서버 (기본 포트 8080)
export PORT=8080
go run ./cmd/apiserver &

curl http://localhost:8080/api/notices
curl http://localhost:8080/api/notices/<id>
```

### 3. 테스트 실행

```bash
cd collector
go test ./... -v
```

## 배포 (Railway / Render 무료 티어)

`render.yaml` 블루프린트 포함 — Render 대시보드에서 "New > Blueprint"로 이 저장소를
선택하면 API 서버 + 수집 데몬 + 무료 Postgres가 한 번에 구성됩니다.

Railway는 저장소 연결 시 `Dockerfile.apiserver`(웹 서비스)와
`Dockerfile.collector`(워커)를 각각 별도 서비스로 등록하고, 두 서비스 모두
Railway에서 제공하는 Postgres 플러그인의 `DATABASE_URL`을 환경변수로 연결하면 됩니다.

무료 티어 정책(가동시간 제한, 슬립 여부, DB 보관기간 등)은 배포 시점에
각 서비스 공식 문서에서 최신 조건을 확인하세요.

## 새 데이터 출처 추가하는 방법

1. `collector/internal/collector/sources/<출처코드>/` 폴더 생성
2. `collector.Collector` 인터페이스 4개 메서드 구현
   (`FetchList`, `FetchDetail`, `FetchAttachments`, `Normalize`)
3. `cmd/collector-daemon/main.go`에서 `demo.New()`를 새 소스로 교체
4. runner/store/changedetect 등 공통 코드는 수정하지 않음
