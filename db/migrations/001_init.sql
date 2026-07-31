-- ============================================================
-- 공공사업 참여판단 데이터 플랫폼 - 초기 스키마
-- 대상 DB: PostgreSQL 16+
-- 원칙: 원본은 수정하지 않는다 / 모든 변경은 버전으로 남긴다
-- ============================================================

CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS pg_trgm;    -- 부분 문자열 검색(초기 검색용)

-- ------------------------------------------------------------
-- 4.1 / 5.1  데이터 출처
-- ------------------------------------------------------------
CREATE TABLE data_sources (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code                TEXT NOT NULL UNIQUE,          -- 예: 'g2b', 'bizinfo'
    name                TEXT NOT NULL,
    organization_name   TEXT,
    source_type         TEXT NOT NULL CHECK (source_type IN ('procurement','support_program')),
    base_url            TEXT NOT NULL,
    uses_api            BOOLEAN NOT NULL DEFAULT true,
    auth_type           TEXT,                          -- 'api_key','oauth','none'
    rate_limit_per_sec  INTEGER NOT NULL DEFAULT 2,
    rate_limit_per_day  INTEGER,
    collection_interval_minutes INTEGER NOT NULL DEFAULT 60,
    is_active           BOOLEAN NOT NULL DEFAULT true,
    last_success_at     TIMESTAMPTZ,
    last_error_at       TIMESTAMPTZ,
    last_error_message  TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------
-- 5.2 수집 작업
-- ------------------------------------------------------------
CREATE TABLE collection_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id       UUID NOT NULL REFERENCES data_sources(id),
    job_type        TEXT NOT NULL CHECK (job_type IN ('full','incremental','revalidate')),
    status          TEXT NOT NULL DEFAULT 'queued'
                        CHECK (status IN ('queued','running','completed','failed','retrying','cancelled','review_required')),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    processed_count INTEGER NOT NULL DEFAULT 0,
    success_count   INTEGER NOT NULL DEFAULT 0,
    failed_count    INTEGER NOT NULL DEFAULT 0,
    retry_count     INTEGER NOT NULL DEFAULT 0,
    error_summary   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_collection_jobs_source_status ON collection_jobs(source_id, status);

-- ------------------------------------------------------------
-- 4.2 원본 저장 계층 (수정 불가, append-only)
-- ------------------------------------------------------------
CREATE TABLE raw_documents (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id           UUID NOT NULL REFERENCES data_sources(id),
    job_id              UUID REFERENCES collection_jobs(id),
    external_notice_id  TEXT,                  -- 출처 기관의 원본 공고번호
    request_url         TEXT NOT NULL,
    response_status     INTEGER,
    raw_content         TEXT NOT NULL,          -- API JSON 또는 HTML 원문
    content_hash        TEXT NOT NULL,          -- sha256(raw_content)
    collected_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    collector_version   TEXT NOT NULL,
    error_detail        TEXT
);
CREATE INDEX idx_raw_documents_hash ON raw_documents(content_hash);
CREATE INDEX idx_raw_documents_source_ext ON raw_documents(source_id, external_notice_id);

-- ------------------------------------------------------------
-- 5.3 공고 기본정보 (정규화 계층)
-- ------------------------------------------------------------
CREATE TABLE notices (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id           UUID NOT NULL REFERENCES data_sources(id),
    external_notice_id  TEXT NOT NULL,
    notice_type         TEXT NOT NULL CHECK (notice_type IN ('procurement','support_program')),
    title               TEXT NOT NULL,
    organization_name   TEXT,
    department_name     TEXT,
    region              TEXT,
    industry            TEXT,
    published_at        DATE,
    application_start_at DATE,
    application_end_at  DATE,
    budget_amount        BIGINT,
    support_amount        BIGINT,
    status              TEXT NOT NULL DEFAULT 'open'
                            CHECK (status IN ('open','closed','cancelled','reannounced')),
    official_url        TEXT,
    current_version     INTEGER NOT NULL DEFAULT 1,
    quality_score       SMALLINT NOT NULL DEFAULT 0,   -- 15.3 품질 점수 (0~100)
    first_collected_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_verified_at    TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_id, external_notice_id)
);
CREATE INDEX idx_notices_search ON notices USING gin (title gin_trgm_ops);
CREATE INDEX idx_notices_filter ON notices(notice_type, region, industry, status);
CREATE INDEX idx_notices_deadline ON notices(application_end_at);

-- ------------------------------------------------------------
-- 5.4 공고 버전 (변경이력의 뼈대 - 원문은 절대 덮어쓰지 않음)
-- ------------------------------------------------------------
CREATE TABLE notice_versions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notice_id       UUID NOT NULL REFERENCES notices(id),
    version_number  INTEGER NOT NULL,
    raw_document_id UUID NOT NULL REFERENCES raw_documents(id),
    collected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    change_type     TEXT NOT NULL DEFAULT 'initial'
                        CHECK (change_type IN ('initial','correction','minor_update','major_update')),
    change_summary  TEXT,
    is_current      BOOLEAN NOT NULL DEFAULT true,
    review_status   TEXT NOT NULL DEFAULT 'pending'
                        CHECK (review_status IN ('pending','auto_approved','reviewed','review_required')),
    -- AI 요약 브리핑(analyzer/ai_summarize.py, claude-sonnet-5) — 공고 상세의
    -- "핵심 3줄 요약". 버전별 1:1 속성이라 별도 테이블이 아니라 컬럼으로 둔다.
    ai_summary_lines        TEXT[],
    ai_summary_model        TEXT,
    ai_summary_generated_at TIMESTAMPTZ,
    UNIQUE (notice_id, version_number)
);
CREATE INDEX idx_notice_versions_current ON notice_versions(notice_id) WHERE is_current;

-- ------------------------------------------------------------
-- 5.5 첨부파일
-- ------------------------------------------------------------
CREATE TABLE attachments (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notice_version_id   UUID NOT NULL REFERENCES notice_versions(id),
    original_filename   TEXT NOT NULL,
    stored_filename      TEXT NOT NULL,           -- 오브젝트 스토리지 키
    file_type           TEXT,
    file_size_bytes      BIGINT,
    file_hash            TEXT NOT NULL,
    download_url         TEXT,
    download_status      TEXT NOT NULL DEFAULT 'pending'
                            CHECK (download_status IN ('pending','downloading','completed','failed')),
    extraction_status     TEXT NOT NULL DEFAULT 'pending'
                            CHECK (extraction_status IN ('pending','processing','completed','failed','unsupported')),
    extracted_text        TEXT,   -- 4단계 텍스트 추출 결과 (analyzer/run_extraction.py)
    extraction_error       TEXT,   -- 실패/미지원 사유
    analysis_status      TEXT NOT NULL DEFAULT 'pending'
                            CHECK (analysis_status IN ('pending','processing','completed','failed')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_attachments_version ON attachments(notice_version_id);
CREATE INDEX idx_attachments_hash ON attachments(file_hash);

-- ------------------------------------------------------------
-- 5.6 변경 이력 (필드 단위)
-- ------------------------------------------------------------
CREATE TABLE notice_changes (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notice_id         UUID NOT NULL REFERENCES notices(id),
    from_version_id   UUID REFERENCES notice_versions(id),
    to_version_id     UUID NOT NULL REFERENCES notice_versions(id),
    changed_field     TEXT NOT NULL,
    old_value         TEXT,
    new_value         TEXT,
    importance        TEXT NOT NULL DEFAULT 'minor'
                          CHECK (importance IN ('critical','major','minor')),
    detected_automatically BOOLEAN NOT NULL DEFAULT true,
    reviewed          BOOLEAN NOT NULL DEFAULT false,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_notice_changes_notice ON notice_changes(notice_id);

-- ------------------------------------------------------------
-- 5.7 자격조건
-- ------------------------------------------------------------
CREATE TABLE eligibility_conditions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notice_version_id UUID NOT NULL REFERENCES notice_versions(id),
    category        TEXT NOT NULL,   -- 지역/업종/업력/매출/면허/직접생산 등
    condition_name  TEXT NOT NULL,
    operator        TEXT NOT NULL,   -- eq, lt, lte, gt, gte, in, not_in
    threshold_value TEXT,
    unit            TEXT,
    is_required     BOOLEAN NOT NULL DEFAULT true,
    source_text     TEXT NOT NULL,   -- 원문 근거 문장
    source_page     INTEGER,
    source_attachment_id UUID REFERENCES attachments(id),
    confidence      NUMERIC(3,2) NOT NULL DEFAULT 1.00,  -- 0.00~1.00
    review_status   TEXT NOT NULL DEFAULT 'pending'
                        CHECK (review_status IN ('pending','confirmed','rejected','review_required')),
    extraction_method TEXT NOT NULL DEFAULT 'rule'
                        CHECK (extraction_method IN ('rule','ai')),
    model_version   TEXT,  -- AI 추출(2차)에 사용한 모델명 — 재현성 확인용, 규칙 기반 행은 NULL
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_eligibility_version ON eligibility_conditions(notice_version_id);

-- ------------------------------------------------------------
-- 5.8 제출서류
-- ------------------------------------------------------------
CREATE TABLE required_documents (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notice_version_id UUID NOT NULL REFERENCES notice_versions(id),
    document_name     TEXT NOT NULL,
    is_required       BOOLEAN NOT NULL DEFAULT true,
    issuing_authority TEXT,
    validity_period   TEXT,
    issue_date_condition TEXT,
    original_or_copy  TEXT,
    signature_required BOOLEAN,
    designated_form   BOOLEAN,
    submission_format TEXT,
    source_text       TEXT,
    source_page          INTEGER,
    source_attachment_id UUID REFERENCES attachments(id),
    confidence           NUMERIC(3,2) NOT NULL DEFAULT 0.70,
    review_status        TEXT NOT NULL DEFAULT 'pending'
                              CHECK (review_status IN ('pending','confirmed','rejected','review_required')),
    extraction_method    TEXT NOT NULL DEFAULT 'rule'
                              CHECK (extraction_method IN ('rule','ai')),
    model_version        TEXT  -- AI 추출(2차)에 사용한 모델명 — 재현성 확인용, 규칙 기반 행은 NULL
);

-- ------------------------------------------------------------
-- 5.9 과업범위
-- ------------------------------------------------------------
CREATE TABLE task_requirements (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notice_version_id UUID NOT NULL REFERENCES notice_versions(id),
    category          TEXT,
    name              TEXT NOT NULL,
    detail            TEXT,
    required_personnel TEXT,
    required_qualification TEXT,
    expected_frequency TEXT,
    onsite_required   BOOLEAN,
    deliverables      TEXT,
    source_text       TEXT
);

-- ------------------------------------------------------------
-- 5.10 계약 위험
-- ------------------------------------------------------------
CREATE TABLE contract_risks (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notice_version_id UUID NOT NULL REFERENCES notice_versions(id),
    category          TEXT,
    clause            TEXT,
    risk_level        TEXT CHECK (risk_level IN ('high','medium','low')),
    description       TEXT,
    needs_confirmation TEXT,
    source_text       TEXT,
    review_status     TEXT NOT NULL DEFAULT 'pending'
);

-- ------------------------------------------------------------
-- 기업 프로필 (11.1) / 사용자
-- ------------------------------------------------------------
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email           TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    role            TEXT NOT NULL DEFAULT 'user'
                        CHECK (role IN ('user','company_admin','analyst','operator','system_admin')),
    plan            TEXT NOT NULL DEFAULT 'free'
                        CHECK (plan IN ('free','pro','pro_promo')),
    email_notifications_enabled BOOLEAN NOT NULL DEFAULT true,
    phone_number    TEXT,
    sms_notifications_enabled BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- user_id는 최초 생성자 참조로만 남는다(과거 호환) — 실제 "누가 이
-- 프로필에 접근 가능한가"는 아래 company_members가 단일 진실 공급원이다
-- (팀기능: 회사=company_profiles 1건에 여러 users가 소속). 알림 설정
-- 3개(email/phone/sms)는 팀기능 이전엔 users에 있었으나 "조직 단위로
-- 공유"하기로 해 여기로 옮겼다(팀기능 스펙) — users의 동일 이름 컬럼은
-- 옛 값이 남아있을 뿐 더 이상 읽히지 않는다.
CREATE TABLE company_profiles (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             UUID NOT NULL REFERENCES users(id),
    business_type       TEXT[],  -- 자유 태그 다중선택 (사업자등록증 업태 여러 줄 등록 반영)
    region               TEXT,
    industry             TEXT[], -- 대분류 다중선택, OR 매칭 (겸업 반영 — 5.7 참고)
    business_age_years   NUMERIC(4,1),
    revenue_amount        BIGINT,
    employee_count        INTEGER,
    company_size          TEXT,   -- 소기업/소상공인/중기업 등
    licenses              TEXT[],
    certifications         TEXT[],
    direct_production_cert BOOLEAN NOT NULL DEFAULT false,
    max_performance_amount  BIGINT, -- 최근 3년 최대 실적
    credit_rating           TEXT,
    email_notifications_enabled BOOLEAN NOT NULL DEFAULT true,
    phone_number            TEXT,
    sms_notifications_enabled BOOLEAN NOT NULL DEFAULT false,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------
-- 팀기능: 한 company_profiles(조직)에 여러 users가 소속. owner는 프로필
-- 생성자(팀원관리/구독관리/전체데이터 쓰기 권한), member는 초대로
-- 합류한 사람(파이프라인 조회+참여만 — 프로필/재무/실적/인력/면허/
-- 지식재산권/구독은 읽기 전용, company_pipeline.go 쪽 핸들러만 그대로
-- 씀). UNIQUE(user_id) — 한 로그인 계정은 항상 조직 하나에만 속한다
-- (여러 회사에 동시 소속되는 시나리오는 이번 범위 밖).
CREATE TABLE company_members (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
    user_id             UUID NOT NULL REFERENCES users(id),
    role                TEXT NOT NULL CHECK (role IN ('owner','member')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id)
);
CREATE INDEX idx_company_members_profile ON company_members(company_profile_id);

-- 이메일 초대 링크(Resend 재사용) — token은 URL에 그대로 노출되므로
-- 추측 불가능한 무작위 값이어야 한다(company_team.go에서 crypto/rand로
-- 생성). expires_at 지나면 accept 시점에 거부(상태를 별도 배치로 미리
-- 'expired'로 돌리지 않음 — displayStatus 패턴처럼 판정 시점에 계산).
CREATE TABLE company_invitations (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
    email               TEXT NOT NULL,
    token               TEXT NOT NULL UNIQUE,
    invited_by_user_id  UUID NOT NULL REFERENCES users(id),
    status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','accepted','expired','cancelled')),
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    accepted_at         TIMESTAMPTZ
);
CREATE INDEX idx_company_invitations_token ON company_invitations(token);
CREATE INDEX idx_company_invitations_profile ON company_invitations(company_profile_id);

-- ------------------------------------------------------------
-- 5.x 적합성 판정 결과 (규칙 엔진 산출물, 근거·재현성 확보)
-- ------------------------------------------------------------
CREATE TABLE eligibility_evaluations (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
    notice_version_id   UUID NOT NULL REFERENCES notice_versions(id),
    condition_id        UUID NOT NULL REFERENCES eligibility_conditions(id),
    result              TEXT NOT NULL CHECK (result IN
                            ('met','not_met','conditionally_met','needs_confirmation','insufficient_data','rule_conflict')),
    reason              TEXT NOT NULL,
    rule_engine_version TEXT NOT NULL,
    evaluated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_eval_company_notice ON eligibility_evaluations(company_profile_id, notice_version_id);

-- ------------------------------------------------------------
-- 제출서류 체크리스트(사용자가 준비 여부를 표시) — "AI 비서" 1단계,
-- 기업 프로필 기준으로 문서별 준비 상태를 기록한다.
-- ------------------------------------------------------------
CREATE TABLE document_checklist_items (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id    UUID NOT NULL REFERENCES company_profiles(id),
    required_document_id  UUID NOT NULL REFERENCES required_documents(id),
    is_checked            BOOLEAN NOT NULL DEFAULT true,
    checked_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (company_profile_id, required_document_id)
);

-- ------------------------------------------------------------
-- 면허·인증 구조화 + 증빙서류 업로드 (3.3/3.4/4) — company_profiles의
-- licenses/certifications TEXT[]는 하위호환으로 유지하고, 이 테이블들이
-- 진실의 원천이 된다. company_documents는 사용자가 업로드한 증빙서류
-- 원본 파일 기록(첨부파일 attachments와 동일한 해시 기반 저장 방식).
-- ------------------------------------------------------------
CREATE TABLE company_documents (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id UUID NOT NULL REFERENCES company_profiles(id),
    original_filename  TEXT NOT NULL,
    stored_filename    TEXT NOT NULL,
    file_type          TEXT NOT NULL,
    file_size_bytes    BIGINT NOT NULL,
    file_hash          TEXT NOT NULL,
    uploaded_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 직원 수(employee_count) 검증 근거(4대보험 사업장 가입자명부 등) —
-- company_profiles가 company_documents보다 먼저 정의되어 순환 참조가
-- 생기므로, 이 3개 컬럼은 CREATE TABLE company_profiles 안이 아니라
-- company_documents 생성 직후 별도 ALTER로 추가한다. 가입자 개인명단은
-- 절대 추출하지 않고(개인정보 최소화), 총 가입자 수만 검증 대상이다.
ALTER TABLE company_profiles ADD COLUMN employee_count_source_document_id UUID REFERENCES company_documents(id);
ALTER TABLE company_profiles ADD COLUMN employee_count_confidence TEXT CHECK (employee_count_confidence IN ('A','B','C','D'));
ALTER TABLE company_profiles ADD COLUMN employee_count_verified_at TIMESTAMPTZ;

-- confidence: A=공식API(추후), B=증빙서류 업로드+사용자확인, C=서류없이
-- 사용자 직접입력, D=AI추출 미확인(추후, 이번 구현엔 미사용 — 후보는
-- 사용자 확인 전까지 DB에 저장되지 않으므로 D 상태 자체가 발생하지 않음).
-- status: 보유/미보유/확인되지않음 3가지를 명확히 구분 — "정보없음"을
-- "미보유"로 자동 처리하지 않는다(입력하는 쪽이 명시적으로 선택).
CREATE TABLE company_licenses (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id    UUID NOT NULL REFERENCES company_profiles(id),
    category              TEXT NOT NULL,
    name                  TEXT NOT NULL,
    registration_number   TEXT,
    issuing_authority     TEXT,
    issued_at             DATE,
    expires_at            DATE,
    applicable_industry   TEXT,
    source_document_id    UUID REFERENCES company_documents(id),
    confidence            TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
    status                TEXT NOT NULL CHECK (status IN ('보유','미보유','확인되지않음')),
    verified_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_company_licenses_profile ON company_licenses(company_profile_id);

CREATE TABLE company_certifications (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id    UUID NOT NULL REFERENCES company_profiles(id),
    category              TEXT NOT NULL,
    name                  TEXT NOT NULL,
    registration_number   TEXT,
    issuing_authority     TEXT,
    issued_at             DATE,
    expires_at            DATE,
    applicable_industry   TEXT,
    source_document_id    UUID REFERENCES company_documents(id),
    confidence            TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
    status                TEXT NOT NULL CHECK (status IN ('보유','미보유','확인되지않음')),
    verified_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_company_certifications_profile ON company_certifications(company_profile_id);

-- ------------------------------------------------------------
-- 재무정보 / 수행실적 / 인력정보 — company_licenses/certifications와
-- 같은 패턴(출처+신뢰도 A~D+증빙연결). 이 3개 테이블엔 면허/인증의
-- status(보유/미보유/확인되지않음) 개념이 없다 — "보유 여부"가 아니라
-- "값 자체"가 있거나 없는 데이터라서, 없는 값은 그냥 NULL이다.
-- 불리언성 필드(tax_delinquent 등)도 NULL=확인 안 됨을 구분하기 위해
-- nullable로 둔다(NOT NULL DEFAULT false로 두면 "확인 안 됨"이 "아니오"로
-- 둔갑함 — 같은 이유로 면허 status를 tri-state로 만든 원칙과 동일).
-- ------------------------------------------------------------
CREATE TABLE company_financials (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
    fiscal_year         INTEGER NOT NULL,
    revenue             BIGINT,
    operating_profit    BIGINT,
    net_income          BIGINT,
    capital             BIGINT,
    total_assets        BIGINT,
    total_liabilities   BIGINT,
    debt_ratio          NUMERIC(6,2),
    current_ratio       NUMERIC(6,2),
    credit_rating       TEXT,
    tax_delinquent      BOOLEAN,
    capital_impairment  BOOLEAN,
    -- 증빙서류 17종 확대: 이 표는 재무제표뿐 아니라 신용평가서/표준재무제표증명/
    -- 부가가치세 과세표준증명도 흡수한다 — 어느 문서로 검증됐는지 구분해서
    -- 보여주기 위한 출처 표시일 뿐, 값 자체의 신뢰도는 기존 confidence(A~D)가
    -- 이미 담당한다(두 컬럼은 서로 다른 축).
    source_document_type TEXT CHECK (source_document_type IN
                            ('재무제표','신용평가서','표준재무제표증명','부가가치세과세표준증명','기타')),
    source_document_id  UUID REFERENCES company_documents(id),
    confidence          TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
    verified_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (company_profile_id, fiscal_year)
);

CREATE TABLE company_track_records (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
    project_name        TEXT NOT NULL,
    client_name         TEXT,
    contract_date       DATE,
    period_start        DATE,
    period_end          DATE,
    contract_amount     BIGINT,
    project_type        TEXT,
    industry_field      TEXT,
    region              TEXT,
    is_joint_venture    BOOLEAN,
    share_ratio         NUMERIC(5,2),
    scope               TEXT,
    core_technology     TEXT,
    is_completed        BOOLEAN,
    -- source_document_type: 이 표는 수행실적증명서 외에 계약서/세금계산서도
    -- 흡수한다(같은 이유로 company_financials에도 추가) — 세금계산서/계약서는
    -- 수행기간·공동수급여부 등 일부 필드가 문서 자체에 없는 경우가 많다는 걸
    -- 프론트가 이 값으로 미리 안내할 수 있게 한다.
    source_document_type TEXT CHECK (source_document_type IN
                            ('수행실적증명서','계약서','세금계산서','기타')),
    source_document_id  UUID REFERENCES company_documents(id),
    confidence          TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
    verified_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_company_track_records_profile ON company_track_records(company_profile_id);

-- ------------------------------------------------------------
-- 지식재산권(특허·상표·디자인·실용신안) — 면허/인증과 달리 "보유 여부"
-- 개념이 아니라 출원~등록~소멸까지 상태가 이어지는 별개 개념이라 새
-- 테이블로 분리한다(면허/인증 status인 보유/미보유/확인되지않음과는
-- 어휘 자체가 다름 — 예를 들어 "출원중"은 미보유가 아니다).
-- ------------------------------------------------------------
CREATE TABLE company_intellectual_property (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id    UUID NOT NULL REFERENCES company_profiles(id),
    ip_type               TEXT NOT NULL CHECK (ip_type IN ('특허','상표','디자인','실용신안')),
    title                 TEXT NOT NULL,
    application_number    TEXT,
    registration_number   TEXT,
    applicant_name        TEXT,
    application_date      DATE,
    registration_date     DATE,
    expires_at            DATE,
    status                TEXT NOT NULL CHECK (status IN ('등록','출원중','거절','소멸','확인필요')),
    source_document_id    UUID REFERENCES company_documents(id),
    confidence            TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
    verified_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_company_ip_profile ON company_intellectual_property(company_profile_id);

-- 개인정보 최소화: 이름/연락처 등 개인식별정보 컬럼을 두지 않는다 —
-- 증빙서류(기술인력현황표 등)에 이름이 있어도 AI 추출 프롬프트에서
-- 명시적으로 제외하고 매칭에 필요한 수준(직무/기술분야/경력/등급)까지만 저장한다.
CREATE TABLE company_personnel (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
    role                TEXT,
    tech_field          TEXT,
    career_years        NUMERIC(4,1),
    tech_grade          TEXT,
    qualifications      TEXT[],
    recent_project      TEXT,
    available_from      DATE,
    source_document_id  UUID REFERENCES company_documents(id),
    confidence          TEXT NOT NULL CHECK (confidence IN ('A','B','C','D')),
    verified_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_company_personnel_profile ON company_personnel(company_profile_id);

-- ------------------------------------------------------------
-- 업무자동화: 참여 파이프라인 — 발견(추천)→참여결정→담당자→서류→일정을
-- 잇는 흐름. UNIQUE(company_profile_id, notice_id)는 "참여 검토" 원클릭
-- API의 멱등성 근거(재클릭해도 중복 생성 안 됨). assignee_name은 자유
-- 텍스트다 — 지금 시스템엔 회사 내 여러 팀원 개념이 없어(company_profiles가
-- user_id 1:1) FK로 만들 대상이 없다.
-- ------------------------------------------------------------
CREATE TABLE notice_pipeline_entries (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
    notice_id           UUID NOT NULL REFERENCES notices(id),
    status              TEXT NOT NULL DEFAULT '검토전'
                            CHECK (status IN ('검토전','참여검토','승인대기','준비중','제출완료','낙찰','탈락','보류','제외')),
    assignee_name       TEXT,
    assignee_email      TEXT,
    assignee_phone      TEXT,
    decided_at          TIMESTAMPTZ,
    submission_deadline DATE,
    memo                TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (company_profile_id, notice_id)
);
CREATE INDEX idx_pipeline_entries_company ON notice_pipeline_entries(company_profile_id);

-- 자동매칭 규칙(company_pipeline.go): required_documents.document_name을
-- company_licenses/certifications.name과 정확 일치로 대조해 status를
-- 채운다. 일치 안 하면 확인필요 — "신규작성" 여부는 시스템이 판단할 수
-- 없어 임의로 단정하지 않는다.
CREATE TABLE pipeline_checklist_items (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pipeline_entry_id     UUID NOT NULL REFERENCES notice_pipeline_entries(id),
    document_name         TEXT NOT NULL,
    status                TEXT NOT NULL DEFAULT '확인필요'
                              CHECK (status IN ('보유','갱신필요','신규작성','발급필요','확인필요')),
    required_document_id  UUID REFERENCES required_documents(id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_pipeline_checklist_entry ON pipeline_checklist_items(pipeline_entry_id);

-- ------------------------------------------------------------
-- 이메일/SMS 알림 발송 이력 — 중복발송 방지(동일 event_type+대상+channel에
-- 대해 status='sent' 행이 이미 있으면 재발송하지 않음)와 발송 성공/실패
-- 기록을 겸한다. pipeline_entry_id/notice_id는 이벤트 종류에 따라
-- 둘 다, 하나만, 또는 둘 다 NULL일 수 있다(다이제스트는 notice_id만,
-- 담당자 알림은 둘 다 채워짐). channel이 email/sms 중 무엇이냐에 따라
-- recipient_email/recipient_phone 중 해당하는 쪽만 채워진다 — 이메일과
-- SMS는 같은 (event_type, pipeline_entry_id, notice_id, user_id) 조합이라도
-- 서로 독립적으로 중복발송 여부를 판단해야 하므로 dedup 인덱스에 channel도
-- 포함한다(한쪽 채널만 먼저 성공해도 다른 채널이 막히면 안 됨).
-- ------------------------------------------------------------
CREATE TABLE notification_log (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type         TEXT NOT NULL CHECK (event_type IN
                            ('deadline_d3','deadline_d1','recommendation_digest','assignee_status_change')),
    channel            TEXT NOT NULL DEFAULT 'email' CHECK (channel IN ('email','sms')),
    recipient_email    TEXT,
    recipient_phone    TEXT,
    user_id            UUID REFERENCES users(id),
    pipeline_entry_id  UUID REFERENCES notice_pipeline_entries(id),
    notice_id          UUID REFERENCES notices(id),
    subject            TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('sent','failed')),
    error_message      TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT notification_log_recipient_check CHECK (recipient_email IS NOT NULL OR recipient_phone IS NOT NULL)
);
CREATE INDEX idx_notification_log_dedup ON notification_log(event_type, pipeline_entry_id, notice_id, user_id, channel);

-- ------------------------------------------------------------
-- 관심공고(북마크) — 로그인 사용자가 공고를 찜해두고 마이페이지에서
-- 모아본다. 기업 프로필과 무관하게 user_id에만 연결된다.
-- ------------------------------------------------------------
CREATE TABLE notice_bookmarks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id),
    notice_id   UUID NOT NULL REFERENCES notices(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, notice_id)
);
CREATE INDEX idx_notice_bookmarks_notice ON notice_bookmarks(notice_id);

-- ------------------------------------------------------------
-- 과금: 분석 크레딧 (무료체험 / 건별결제 / 구독 잔여건)
-- ------------------------------------------------------------
CREATE TABLE analysis_credits (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id),
    credit_type     TEXT NOT NULL CHECK (credit_type IN ('free_trial','subscription','single_purchase')),
    source_ref      TEXT,           -- 결제ID 또는 구독 사이클 ID
    granted_count   INTEGER NOT NULL,
    used_count      INTEGER NOT NULL DEFAULT 0,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE credit_usage_log (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    credit_id       UUID NOT NULL REFERENCES analysis_credits(id),
    user_id         UUID NOT NULL REFERENCES users(id),
    notice_id       UUID REFERENCES notices(id),
    action          TEXT NOT NULL,  -- 'detail_report','pdf','company_match' 등
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------
-- 감사 로그 (16.3)
-- ------------------------------------------------------------
CREATE TABLE audit_logs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_user_id UUID REFERENCES users(id),
    action        TEXT NOT NULL,
    target_type   TEXT,
    target_id     TEXT,
    detail        JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_logs_actor ON audit_logs(actor_user_id, created_at);

-- ------------------------------------------------------------
-- 결제/구독 (토스페이먼츠, OAuth 없는 결제창 API 개별연동 방식 — billing.go 참고)
--
-- subscriptions는 company_profile_id당 1행(UNIQUE)만 유지한다.
-- checkout(POST /api/billing/checkout)은 이 row가 존재하지 않을 때만
-- (최초 가입 직후 등) status='pending'으로 하나 만들어둔다 —
-- payment_log.subscription_id가 NOT NULL이라 confirm이 실패해도 로그를
-- 남길 대상 row가 항상 있어야 하기 때문. **이미 row가 있으면(과거에
-- active였든 뭐든) checkout은 절대 건드리지 않는다** — 그렇지 않으면
-- 이미 결제를 완료한 사용자가 다른 플랜의 "구독하기"만 눌러도(새 결제가
-- 완료되기도 전에) 기존 유료 구독이 즉시 pending으로 떨어지는 회귀가
-- 생긴다(실제로 발생했던 버그). plan/status 전환은 오직
-- handleBillingConfirm이 토스 승인 API에서 성공 응답을 받았을 때만
-- 일어난다. status='pending'/'cancelled'/'expired'인 동안은 기능 제한
-- 로직(billing.go의 effectivePlan)이 free 플랜과 동일하게 취급한다.
-- ------------------------------------------------------------
CREATE TABLE subscriptions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id  UUID NOT NULL REFERENCES company_profiles(id),
    plan                TEXT NOT NULL DEFAULT 'free'
                            CHECK (plan IN ('free','basic','pro','business')),
    status              TEXT NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('active','cancelled','expired','pending')),
    billing_key         TEXT, -- 정기결제(다음 단계) 전까지는 항상 NULL
    started_at          TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ,
    amount              BIGINT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (company_profile_id)
);

-- status: '승인'/'실패'/'취소' 3가지뿐 — "요청됨/대기" 상태는 이 표에
-- 남기지 않는다(체크아웃 시점엔 아직 Toss에 아무것도 요청 안 함, 주문
-- 식별자만 그때그때 생성). confirm 핸들러가 Toss 승인 API를 호출한
-- 결과가 나온 뒤에만 이 표에 행이 생긴다.
CREATE TABLE payment_log (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id    UUID NOT NULL REFERENCES subscriptions(id),
    toss_payment_key   TEXT NOT NULL,
    toss_order_id      TEXT NOT NULL,
    amount             BIGINT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('승인','실패','취소')),
    requested_at       TIMESTAMPTZ NOT NULL,
    approved_at        TIMESTAMPTZ,
    raw_response       JSONB,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_payment_log_subscription ON payment_log(subscription_id);

-- ------------------------------------------------------------
-- updated_at 자동 갱신 트리거
-- ------------------------------------------------------------
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_notices_updated_at BEFORE UPDATE ON notices
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_data_sources_updated_at BEFORE UPDATE ON data_sources
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_company_profiles_updated_at BEFORE UPDATE ON company_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_subscriptions_updated_at BEFORE UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
