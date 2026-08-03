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
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- extracted_text가 채워진 뒤(analyzer/run_extraction.py, 여전히 수동)
    -- Phase 4 규칙기반 추출(Go, apiserver 백그라운드 배치)이 이 첨부파일을
    -- 처리했는지 표시 — NULL이면 다음 배치에서 다시 시도한다. 매 시간
    -- 전체 completed 첨부를 재스캔하지 않기 위한 워터마크.
    section_extraction_processed_at TIMESTAMPTZ
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
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Phase 4: review_required 행에 AI 보완 추출(Go, 자동 배치)을 시도했는지
    -- 표시. NULL인 행만 다음 배치 대상 — 안 찍으면 review_required 상태가
    -- 사람 검토 전까지 유지되는 한(review.go), 매시간 계속 재호출돼 비용이
    -- 낭비된다.
    ai_supplement_attempted_at TIMESTAMPTZ,
    -- Phase 4 2단계: 실패할 때마다 증가. maxAISupplementAttempts(Go 상수)에
    -- 도달하면 성공 못 해도 ai_supplement_attempted_at을 찍어 무한 재시도를
    -- 막는다(document_extraction.go 참고).
    ai_supplement_attempts INTEGER NOT NULL DEFAULT 0
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
    model_version        TEXT,  -- AI 추출(2차)에 사용한 모델명 — 재현성 확인용, 규칙 기반 행은 NULL
    ai_supplement_attempted_at TIMESTAMPTZ,  -- eligibility_conditions와 동일 목적(위 주석 참고)
    ai_supplement_attempts INTEGER NOT NULL DEFAULT 0  -- eligibility_conditions와 동일 목적(위 주석 참고)
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
    password_hash   TEXT, -- 소셜 로그인(간편로그인) 전용 계정은 비밀번호를 만든 적이 없어 NULL(원래는 NOT NULL이었으나 ensureOAuthIdentitiesTable에서 완화). handleLogin은 NULL이면 social_login_only 에러 반환
    role            TEXT NOT NULL DEFAULT 'user'
                        CHECK (role IN ('user','company_admin','analyst','operator','system_admin')),
    plan            TEXT NOT NULL DEFAULT 'free'
                        CHECK (plan IN ('free','pro','pro_promo')),
    email_notifications_enabled BOOLEAN NOT NULL DEFAULT true,
    phone_number    TEXT,
    sms_notifications_enabled BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at   TIMESTAMPTZ, -- 관리자 화면(admin.go)의 회원목록용. 이 컬럼이 생기기 전 로그인은 기록이 없어 NULL로 남는다(과거 소급 불가).
    deactivated_at  TIMESTAMPTZ -- 관리자 회원 탈퇴 처리(admin_member_actions.go) 표식. NOT NULL이면 로그인 영구 차단(handleLogin/handleOAuthCallback). 탈퇴 시 email/password_hash/phone_number도 익명화되지만 행 자체는 지우지 않는다(payment_log/audit_logs FK 참조 + 법적 보관기간 고려).
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
    email_notifications_enabled BOOLEAN NOT NULL DEFAULT true, -- 추천 공고 다이제스트 전용(마감 리마인더/상태변경 알림은 company_contacts 담당자별 설정으로 이전됨)
    phone_number            TEXT, -- 사업자 대표전화번호(회사 단위). 개인 휴대폰번호는 users.phone_number. 한동안 조직 알림 채널로도 쓰이다 담당자별 SMS(company_contacts)로 대체돼 미사용이었으나, 회원가입 2단계(업체정보)에서 필수 항목으로 다시 쓰기 시작함
    sms_notifications_enabled BOOLEAN NOT NULL DEFAULT false, -- 더 이상 알림에 쓰이지 않음(위와 동일한 이유로 컬럼만 유지)
    notification_days_before INTEGER[] NOT NULL DEFAULT '{3,1}', -- 제출마감 리마인더 D-N 선택(7/3/1 중 다중선택)
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 관리자 개별 회원 한도조정(#/admin/members/{id}, admin_member_actions.go).
    -- custom_ai_analysis_limit_month는 'YYYY-MM' 문자열 — 이 값이 조회 시점의
    -- 현재 월과 일치할 때만 오버라이드가 적용되고, 달이 바뀌면 별도 배치 없이
    -- 자동으로 플랜 기본 한도로 되돌아간다(api.effectiveAIAnalysisLimit).
    custom_ai_analysis_limit        INTEGER,
    custom_ai_analysis_limit_month  TEXT,
    custom_ai_analysis_limit_reason TEXT
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
    document_kind       TEXT,  -- 어느 업로드 엔드포인트가 만들었는지(license_or_certification/financial/track_record/personnel/intellectual_property/employee_verification) — AI 사용내역 화면에서 "어떤 서류" 표시용
    extraction_status  TEXT CHECK (extraction_status IN ('success','failed')),  -- NULL=처리중(정상 흐름에서는 순간적). AI 분석 한도는 'success'인 행만 카운트(countAIAnalysisThisMonth, billing.go) — 실패는 어떤 이유든 한도를 차감하지 않는다(2026-08-03 정책)
    failure_reason     TEXT,  -- 실패 시 사용자 친화적 문구만 저장(원본 API 에러 메시지 노출 안 함) — company_documents.go의 classifyExtractionFailureReason 참고
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
    awarded_amount      BIGINT, -- 성장분석(ROI) 근거 — status='낙찰'일 때 사용자가 직접 입력하는 실제 낙찰금액(공고 budget_amount와 다를 수 있어 별도 필드)
    company_profile_snapshot JSONB, -- 원클릭 참여검토(Phase 1) 시점의 company_profiles 행 전체 스냅샷(감사용)
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
-- 담당자 관리 — 참여 검토(파이프라인 생성) 시 assignee_name/email/phone을
-- 매번 빈칸부터 입력하지 않도록, 회사가 미리 등록해두는 담당자 목록.
-- is_default는 "자동 채우기에 쓸 한 명"을 가리키며, 한 번에 하나만
-- true가 되도록 애플리케이션 코드(company_contacts.go)가 트랜잭션으로
-- 보장한다(DB 제약이 아님 — 어차피 "새로 지정하면 기존 것 해제"하는
-- 트랜잭션이 필요해 부분 유니크 인덱스를 추가로 걸 실익이 적다).
--
-- {email,sms,push}_notifications_enabled — 알림 수신 여부를 조직 공용
-- 설정(company_profiles.phone_number/sms_notifications_enabled, 더 이상
-- 안 씀) 대신 담당자 개인 단위로 둔다. push는 인프라가 아직 없어 항상
-- false로 시작(나중에 실제로 붙일 때 이 컬럼만 켜면 되도록 미리 만듦).
-- notification_log가 이 테이블을 참조하므로(contact_id) 아래
-- notification_log보다 먼저 있어야 한다.
-- ------------------------------------------------------------
CREATE TABLE company_contacts (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id UUID NOT NULL REFERENCES company_profiles(id),
    name               TEXT NOT NULL,
    email              TEXT,
    phone              TEXT,
    is_default         BOOLEAN NOT NULL DEFAULT false,
    email_notifications_enabled BOOLEAN NOT NULL DEFAULT true,
    sms_notifications_enabled   BOOLEAN NOT NULL DEFAULT false,
    push_notifications_enabled  BOOLEAN NOT NULL DEFAULT false,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_company_contacts_profile ON company_contacts(company_profile_id);

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
                            ('deadline_d7','deadline_d3','deadline_d1','recommendation_digest','assignee_status_change',
                             'weekly_report','monthly_report','team_invite','team_invite_accepted','admin_broadcast')),
    channel            TEXT NOT NULL DEFAULT 'email' CHECK (channel IN ('email','sms')),
    recipient_email    TEXT,
    recipient_phone    TEXT,
    user_id            UUID REFERENCES users(id), -- 추천 공고 다이제스트(회원 단위)/팀초대수락(초대한 사람)만 채움
    contact_id         UUID REFERENCES company_contacts(id), -- 마감 리마인더/상태변경 알림(담당자 단위)만 채움
    pipeline_entry_id  UUID REFERENCES notice_pipeline_entries(id),
    notice_id          UUID REFERENCES notices(id),
    subject            TEXT NOT NULL,
    -- 'skipped_quota': Free 플랜 월간 알림성 이메일 한도 초과로 이메일
    -- 채널만 조용히 생략(인앱/푸시는 정상 발송) — notifications.go의
    -- checkEmailNotificationQuota 참고.
    status             TEXT NOT NULL CHECK (status IN ('sent','failed','skipped_quota')),
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
-- 이용약관/개인정보처리방침 동의 기록 + 버전 관리(회원가입 개선, legal_documents.go/signup_agreement.go)
-- ------------------------------------------------------------
CREATE TABLE terms_agreements (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id),
    terms_version   TEXT NOT NULL,  -- 동의 시점의 legal_documents(type='terms') 활성 버전 — 클라이언트가 아니라 서버가 그 순간 조회해서 기록(변조 방지)
    privacy_version TEXT NOT NULL,  -- 위와 동일, type='privacy'
    agreed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    ip_address      TEXT  -- 선택 정보(법적 증빙 보조용). X-Forwarded-For 기반이라 위조 가능성이 있어 보안 판단에는 안 씀
);
CREATE INDEX idx_terms_agreements_user ON terms_agreements(user_id, agreed_at);

-- type('terms'/'privacy')별로 is_active=true인 행이 항상 최대 1개인 것은
-- DB 제약이 아니라 애플리케이션(handleAdminPublishLegalDocument)이
-- 트랜잭션으로 보장한다. 이전 버전은 삭제되지 않고 이력으로 남는다.
-- content 안의 {brand_name}/{company_name}/{business_registration_number}/
-- {representative_name}/{address}/{main_phone}/{contact_email} 토큰은
-- 조회 시점에 company_info 값으로 치환된다(중복 관리 방지).
--
-- ⚠️ 초기 시드 콘텐츠(collector/internal/migrate/legal_documents_seed.go)는
-- 표준 템플릿 기반 초안이며, 실제 발행 전 반드시 법률 전문가 검토가 필요하다.
CREATE TABLE legal_documents (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type           TEXT NOT NULL CHECK (type IN ('terms','privacy')),
    version        TEXT NOT NULL,
    content        TEXT NOT NULL,
    effective_date DATE NOT NULL,
    is_active      BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_legal_documents_type_active ON legal_documents(type, is_active);

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
    pending_plan        TEXT CHECK (pending_plan IN ('free','basic','pro','business')), -- 예약 다운그레이드: 결제는 완료됐지만 expires_at까지는 기존 plan 혜택을 유지하고, 그 이후 배치(ApplyScheduledDowngrades)가 이 값으로 전환한다. 즉시 적용되는 업그레이드는 이 컬럼을 안 씀(NULL 유지).
    -- 해지(구독취소, 환불과 다름 — billing.go 주석 참고): pending_plan과
    -- 별개 필드로 둔다. pending_plan은 "다음 결제를 이미 낸" 유료→유료
    -- 예약 전환 전용이라 결제 없이 무료로 전환하는 해지엔 안 맞는다
    -- (ApplyScheduledDowngrades가 매번 다음 만료일을 1개월 뒤로 다시
    -- 계산하는 등 유료 갱신을 전제로 함). true면 expires_at 도달 시
    -- ApplyScheduledCancellations 배치가 즉시 Free로 전환한다.
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT false,
    cancel_requested_at TIMESTAMPTZ,
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
    status             TEXT NOT NULL CHECK (status IN ('승인','실패','취소','환불')),
    requested_at       TIMESTAMPTZ NOT NULL,
    approved_at        TIMESTAMPTZ,
    payment_method     TEXT, -- 토스 응답 method 그대로("카드"/"가상계좌"/"계좌이체" 등, 실패 건은 NULL)
    raw_response       JSONB,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 환불(서비스 미사용 전액환불만 허용, billing.go handleBillingRefundRequest
    -- 참고) 처리 시 이 승인(status='승인') 행을 '환불'로 갱신하면서 채운다.
    -- 새 행을 만들지 않고 원본 결제 행 자체를 갱신 — 사용자 요청 원문의
    -- "payment_log 상태를 갱신" 표현 그대로.
    refund_reason      TEXT,
    refunded_at        TIMESTAMPTZ,
    refund_processed_by TEXT -- 현재는 항상 'system_auto'(자동 판정) — 수동 환불 경로 없음
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

-- ------------------------------------------------------------
-- 경쟁사/낙찰이력 — 조달청 나라장터 낙찰정보서비스(ScsbidInfoService)
-- 연동 예정(수집기는 API 활용신청 승인 후 별도 추가 — 이 테이블/조회
-- 로직은 데이터가 비어 있어도 "아직 수집된 낙찰 이력이 없습니다"로
-- 정상 동작한다). notice_id로 직접 연결하지 않는다 — 낙찰이력은 우리가
-- 수집한 공고와 무관하게 독립적으로 쌓이는 과거 데이터라(우리 notices에
-- 없는 발주기관 이력도 있음), organization_name/industry로만 매칭한다.
-- ------------------------------------------------------------
CREATE TABLE notice_award_history (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id         UUID NOT NULL REFERENCES data_sources(id),
    external_bid_id   TEXT NOT NULL,  -- 입찰공고번호+차수(bidNtceNo+bidNtceOrd)
    organization_name TEXT NOT NULL,
    industry          TEXT,
    title             TEXT,
    winner_name       TEXT,
    award_amount      BIGINT,
    award_rate        NUMERIC(6,3),  -- 낙찰률(%)
    budget_amount     BIGINT,
    opened_at         DATE,
    raw_payload       TEXT,
    collected_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_id, external_bid_id)
);
CREATE INDEX idx_award_history_org ON notice_award_history(organization_name);
CREATE INDEX idx_award_history_industry ON notice_award_history(industry);

-- ------------------------------------------------------------
-- 주간/월간 자동 리포트 — 매주 월요일(주간)/매월 1일(월간) 09:00 KST
-- 배치가 채운다(reports.go). 한 테이블에 period_type으로 구분한다
-- (weekly_reports/monthly_reports로 나누면 스키마가 완전히 동일해서
-- 그냥 중복이 된다). summary는 그 시점에 계산한 값의 스냅샷이라
-- JSONB로 통째로 저장 — 나중에 집계 기준이 바뀌어도 과거 리포트는
-- 그대로 보존된다. UNIQUE(company_profile_id, period_type,
-- period_start)가 배치 재실행 시 멱등성 근거(같은 기간 리포트 중복
-- 생성 안 됨).
-- ------------------------------------------------------------
CREATE TABLE reports (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id UUID NOT NULL REFERENCES company_profiles(id),
    period_type        TEXT NOT NULL CHECK (period_type IN ('weekly','monthly')),
    period_start       DATE NOT NULL,
    period_end         DATE NOT NULL,
    summary            JSONB NOT NULL,
    generated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (company_profile_id, period_type, period_start)
);
CREATE INDEX idx_reports_profile ON reports(company_profile_id, period_start DESC);

-- ------------------------------------------------------------
-- 인앱 알림함 — notification_log(이메일/SMS 발송 이력, 채널별로 행이
-- 갈라짐)와는 별개다. 여기는 "사용자가 화면에서 보는 알림 목록" 전용이라
-- 채널 상관없이 이벤트 1건당 딱 1행만 쌓는다(같은 마감 리마인더가 이메일+
-- SMS 두 통 나가도 인앱 알림함엔 한 번만 뜬다). company_profile_id는
-- 조직 단위 이벤트(마감 리마인더/상태변경 — 같은 회사 팀원 전체에게
-- 보임), user_id는 회원 단위 이벤트(추천 공고 다이제스트 — 받은 회원
-- 본인에게만 보임)에 채운다. 정확히 하나만 채워지는 게 정상.
-- ------------------------------------------------------------
CREATE TABLE in_app_notifications (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_profile_id UUID REFERENCES company_profiles(id),
    user_id            UUID REFERENCES users(id),
    event_type         TEXT NOT NULL,
    title              TEXT NOT NULL,
    body               TEXT NOT NULL,
    pipeline_entry_id  UUID REFERENCES notice_pipeline_entries(id),
    notice_id          UUID REFERENCES notices(id),
    read_at            TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT in_app_notifications_scope_check CHECK (
        (company_profile_id IS NOT NULL AND user_id IS NULL) OR
        (company_profile_id IS NULL AND user_id IS NOT NULL)
    )
);
CREATE INDEX idx_in_app_notifications_profile ON in_app_notifications(company_profile_id, created_at DESC);
CREATE INDEX idx_in_app_notifications_user ON in_app_notifications(user_id, created_at DESC);
-- 배치가 하루에 여러 번(수동 재실행 포함) 돌아도 중복 적재되지 않게
-- 막는 dedup은 DB 제약이 아니라 애플리케이션 코드(insertInAppNotification
-- 호출부)가 EXISTS 조회로 판단한다 — 이벤트 종류마다 "같은 알림"의 기준이
-- 달라서(마감 리마인더는 event_type+entry, 상태변경은 event_type+entry+
-- 새 상태값, 다이제스트는 event_type+user+날짜) 단일 UNIQUE 인덱스로는
-- 표현이 안 된다. notification_log의 기존 EXISTS dedup 패턴과 동일.

-- ------------------------------------------------------------
-- Phase 6(웹푸시/PWA) — 구독 단위는 "회원"(로그인 계정)이다. 담당자
-- (company_contacts)가 아니다 — 이메일/SMS는 로그인 없이도 존재하는
-- 담당자 연락처로 보내지만, 웹 푸시는 "로그인해서 브라우저 권한을
-- 승인한 특정 기기"에만 보낼 수 있어 구조가 다르다. endpoint UNIQUE로
-- 같은 기기가 다른 계정으로 재구독하면 소유자가 자연스럽게 갈아치워진다.
-- ------------------------------------------------------------
CREATE TABLE push_subscriptions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id),
    endpoint   TEXT NOT NULL UNIQUE,
    p256dh_key TEXT NOT NULL,
    auth_key   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_push_subscriptions_user ON push_subscriptions(user_id);

-- ------------------------------------------------------------
-- 관리자가 재배포 없이 조절하는 런타임 설정값. 첫 사용처는
-- free_plan_email_limit(Free 플랜 월간 알림성 이메일 한도, 기본 20) —
-- notifications.go의 checkEmailNotificationQuota가 읽는다. 값 검증(숫자
-- 여부 등)은 애플리케이션 레벨에서 하고, 이 테이블 자체는 범용 key-value.
-- ------------------------------------------------------------
CREATE TABLE system_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO system_settings (key, value) VALUES ('free_plan_email_limit', '20');

-- 플랜별 한도/가격(#/admin/plan-settings, api/plan_settings.go의
-- planOverridesByPlan) — 값이 없을 때의 기본값(billing.Plans, billing/plan.go)과
-- 정확히 같은 값으로 시드해 이 기능이 생기기 전과 동일하게 시작한다.
INSERT INTO system_settings (key, value) VALUES
    ('free_pipeline_limit', '3'),
    ('free_ai_analysis_limit', '0'),
    ('basic_ai_analysis_limit', '5'),
    ('basic_price_krw', '19900'),
    ('pro_ai_analysis_limit', '20'),
    ('pro_price_krw', '49000'),
    ('business_ai_analysis_limit', '60'),
    ('business_price_krw', '99000'),
    ('business_member_limit', '3');

-- ------------------------------------------------------------
-- 간편로그인(구글/네이버/카카오). users에 provider/provider_id 컬럼을
-- 직접 두지 않고 별도 테이블로 뺀 이유는 한 사용자가 여러 소셜 계정을
-- 동시에 연결할 수 있게 하기 위함(예: 구글로 가입한 뒤 나중에 카카오도
-- 연결). 이메일이 같은 기존 계정이 있으면 새 유저를 만들지 않고 이
-- 테이블에 행만 추가해 그 계정에 연결한다(oauth_login.go의
-- resolveOAuthUser). users.password_hash는 이제 NULL 허용 — 소셜 전용
-- 계정(비밀번호를 만든 적 없는 계정)에 필요.
-- ------------------------------------------------------------
CREATE TABLE user_oauth_identities (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider         TEXT NOT NULL CHECK (provider IN ('google','naver','kakao')),
    provider_user_id TEXT NOT NULL,
    email            TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_user_id)
);
CREATE INDEX idx_user_oauth_identities_user ON user_oauth_identities(user_id);

-- ------------------------------------------------------------
-- 홈 화면 배너 슬라이드(관리자 CMS 1번). banners.go의 handleListBanners가
-- 공개 API(로그인 불필요)로 읽는다 — 마케팅 화면(비로그인 방문자)에도
-- 노출되기 때문. 이미지는 지금은 collector/internal/webui/static/banners/
-- 아래 고정 SVG 플레이스홀더를 가리키고, 관리자 CMS(3단계, #/admin/banners)
-- 완성 후 실제 업로드 이미지로 교체 가능.
-- ------------------------------------------------------------
CREATE TABLE banners (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title         TEXT NOT NULL,
    image_url     TEXT NOT NULL,
    link_url      TEXT,
    display_order INTEGER NOT NULL DEFAULT 0,
    is_active     BOOLEAN NOT NULL DEFAULT true,
    starts_at     TIMESTAMPTZ,
    ends_at       TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------
-- 관리자 CMS 5번(팝업 관리) — 홈 화면 진입 시 배너보다 우선 노출되는
-- 공지형 오버레이. image_url은 선택(텍스트만 있는 팝업 가능). "오늘 하루
-- 보지 않기"는 서버에 상태를 안 두고 클라이언트 localStorage
-- (popup_dismissed_{id}_{date})로만 처리한다.
-- ------------------------------------------------------------
CREATE TABLE popups (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title      TEXT NOT NULL,
    image_url  TEXT,
    content    TEXT NOT NULL,
    is_active  BOOLEAN NOT NULL DEFAULT true,
    starts_at  TIMESTAMPTZ,
    ends_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------
-- 관리자 CMS 6번(공지 게시판). 사용자용 #/announcements(목록/상세, 조회
-- 시 view_count 증가)와 관리자용 #/admin/announcements(CRUD)가 이 테이블
-- 하나를 공유한다 — 비공개/임시저장 개념이 없다.
-- ------------------------------------------------------------
CREATE TABLE announcements (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title      TEXT NOT NULL,
    content    TEXT NOT NULL,
    is_pinned  BOOLEAN NOT NULL DEFAULT false,
    view_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_announcements_pinned_created ON announcements(is_pinned DESC, created_at DESC);

-- ------------------------------------------------------------
-- 관리자 CMS 4번(회원 알림 메시지) 발송 이력. 실제 발송은 기존 알림
-- 인프라(notify.Client/in_app_notifications/push_notifications.go)를
-- 재사용하고, 이 테이블은 "언제 누구에게 무엇을 보냈는지"만 남긴다.
-- ------------------------------------------------------------
CREATE TABLE broadcast_messages (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title           TEXT NOT NULL,
    content         TEXT NOT NULL,
    target_plan     TEXT, -- NULL = 전체 회원, 값 있으면 free/basic/pro/business 중 하나만 대상
    channels        TEXT[] NOT NULL,
    recipient_count INTEGER NOT NULL DEFAULT 0,
    created_by      UUID NOT NULL REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------
-- 랜딩페이지 푸터/사이트 전체 브랜드명에 쓰이는 회사 정보. 항상 정확히
-- 1행(id=1, CHECK로 강제 — 싱글턴). brand_name만 NOT NULL(사이트
-- 곳곳에 항상 뭔가는 표시돼야 해서 비워둘 수 없음, 기본값은 지금 쓰는
-- 가칭). 나머지 7개(회사정보)는 전부 NULL 허용 — 비워두면 랜딩페이지
-- 푸터에서 항목별로(그리고 전부 비면 블록 전체가) 숨겨진다.
-- ------------------------------------------------------------
CREATE TABLE company_info (
    id                            INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    brand_name                    TEXT NOT NULL DEFAULT '공공사업 AI 비서',
    company_name                  TEXT,
    business_registration_number  TEXT,
    representative_name           TEXT,
    address                       TEXT,
    main_phone                    TEXT,
    contact_email                 TEXT,
    partnership_email             TEXT,
    patent_number                 TEXT,
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO company_info (id) VALUES (1);
