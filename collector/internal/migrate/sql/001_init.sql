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
                        CHECK (review_status IN ('pending','confirmed','rejected')),
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
    source_text       TEXT
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
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

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
