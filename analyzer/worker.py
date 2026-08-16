#!/usr/bin/env python3
"""운영 Document Extraction Worker — 첨부파일 텍스트 추출 consumer.

배경(2026-08-16 진단): 첨부 다운로드(collector)와 구조화 추출(apiserver Go 배치)은
운영에서 돌지만, 그 사이의 "PDF/HWP/HWPX → attachments.extracted_text" 단계는
run_extraction.py를 사람이 로컬에서 수동 실행하는 구조뿐이라 운영 첨부가 전부
extraction_status='pending'(화면상 "텍스트 추출 중")으로 영원히 남아 있었다.
이 워커는 그 단계를 별도 컨테이너(Dockerfile.extractor)로 상시 실행한다.

구조:
    PostgreSQL attachments(작업 큐 겸용, 새 큐 테이블/브로커 없음)
      → claim(pending→processing, FOR UPDATE SKIP LOCKED, 짧은 트랜잭션)
      → download_url 재다운로드(SSRF-safe; 운영 로컬 디스크 파일은 ephemeral라 신뢰 안 함)
      → 매직바이트 검사 → run_extraction.py의 기존 파서(pdfplumber/hwp5txt/hwpx/openpyxl)
      → completed(+extracted_text) / failed / unsupported
      → 이후는 기존 apiserver 60분 배치(section_extraction 등)가 그대로 이어받는다.

실행:
    상시:   DATABASE_URL=... python worker.py
    1회:    python worker.py --once
    백필:   python worker.py --attachment-id ID [--attachment-id ID2] | --notice-id ID
                             [--limit N] [--created-after 2026-08-16T00:00:00Z]
            (백필 옵션이 하나라도 있으면 1회 실행이며 EXTRACTOR_PROCESS_EXISTING 컷오프를 무시)

환경변수(플랫폼 독립 — DATABASE_URL 외에는 전부 선택):
    DATABASE_URL                       필수
    EXTRACTOR_POLL_SECONDS             큐가 비었을 때 대기(기본 10)
    EXTRACTOR_MAX_ATTEMPTS             claim 상한(기본 3) — 넘으면 failed
    EXTRACTOR_STALE_MINUTES            processing이 이보다 오래되면 워커 사망으로 보고 복구(기본 30)
    EXTRACTOR_RETRY_BACKOFF_MINUTES    일시 실패(타임아웃/429/5xx) 재시도 간격(기본 10)
    EXTRACTOR_MAX_DOWNLOAD_BYTES       다운로드 상한(기본 60MB)
    EXTRACTOR_DOWNLOAD_TIMEOUT_SECONDS 다운로드 타임아웃(기본 60)
    EXTRACTOR_PARSE_TIMEOUT_SECONDS    파싱 상한(기본 300) — 초과 시 failed(비정상 파일 1건이 워커를 멈추지 않게)
    EXTRACTOR_PROCESS_EXISTING         true면 과거 pending 전부 대상. 기본 false = 워커 시작
                                       시각(또는 EXTRACTOR_CREATED_AFTER) 이후 생성된 첨부만
                                       — 배포 직후 수천 건 폭주 방지 안전장치. 과거분은 백필 옵션으로
    EXTRACTOR_CREATED_AFTER            컷오프 시각(ISO8601). PROCESS_EXISTING=false일 때 시작 시각 대신 사용
    EXTRACTOR_ALLOW_PRIVATE_URLS       테스트 전용. true면 호스트 allowlist/내부 IP 차단을 끈다(운영 금지)
"""
import argparse
import ipaddress
import logging
import os
import signal
import socket
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone

import psycopg2
import psycopg2.extras

# run_extraction.py의 파서를 그대로 재사용한다(전면 재작성 금지). 이 파일과 같은
# 디렉터리에 있어야 한다(로컬 analyzer/, 컨테이너 /app/).
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from run_extraction import EXTRACTORS, SUPPORTED_TYPES, ExtractionError  # noqa: E402

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger("extraction-worker")


# ---------------------------------------------------------------------------
# 설정
# ---------------------------------------------------------------------------

def _env_int(name, default):
    v = os.environ.get(name, "").strip()
    return int(v) if v else default


def _env_bool(name, default=False):
    v = os.environ.get(name, "").strip().lower()
    if not v:
        return default
    return v in ("1", "true", "yes", "on")


class Config:
    def __init__(self):
        self.dsn = os.environ.get("DATABASE_URL", "")
        self.poll_seconds = _env_int("EXTRACTOR_POLL_SECONDS", 10)
        self.max_attempts = _env_int("EXTRACTOR_MAX_ATTEMPTS", 3)
        self.stale_minutes = _env_int("EXTRACTOR_STALE_MINUTES", 30)
        self.retry_backoff_minutes = _env_int("EXTRACTOR_RETRY_BACKOFF_MINUTES", 10)
        self.max_download_bytes = _env_int("EXTRACTOR_MAX_DOWNLOAD_BYTES", 60 * 1024 * 1024)
        self.download_timeout = _env_int("EXTRACTOR_DOWNLOAD_TIMEOUT_SECONDS", 60)
        self.parse_timeout = _env_int("EXTRACTOR_PARSE_TIMEOUT_SECONDS", 300)
        self.process_existing = _env_bool("EXTRACTOR_PROCESS_EXISTING", False)
        self.created_after = os.environ.get("EXTRACTOR_CREATED_AFTER", "").strip() or None
        self.allow_private_urls = _env_bool("EXTRACTOR_ALLOW_PRIVATE_URLS", False)


# ---------------------------------------------------------------------------
# SSRF-safe 다운로드 (collector/internal/api/attachment_preview.go ssrfSafeFetch와 동일 원칙)
# ---------------------------------------------------------------------------

# 서버가 대신 요청해도 되는 첨부 출처. download_url은 우리가 수집해 저장한 값이라 이미
# 한정적이지만 방어적으로 정부 도메인 suffix만 허용한다(Go previewAllowedHostSuffixes 동일).
ALLOWED_HOST_SUFFIXES = (".g2b.go.kr", ".bizinfo.go.kr", ".go.kr")
MAX_REDIRECTS = 5
USER_AGENT = "biz-platform-extraction-worker/1.0"


class DownloadError(Exception):
    """transient=True면 재시도 가치가 있는 오류(타임아웃/429/5xx/연결), False면 영구."""

    def __init__(self, message, transient=False):
        super().__init__(message)
        self.transient = transient


def host_allowed(host):
    h = (host or "").strip().lower()
    if not h:
        return False
    for suf in ALLOWED_HOST_SUFFIXES:
        if h.endswith(suf) or h == suf.lstrip("."):
            return True
    return False


def is_blocked_ip(ip):
    """사설/loopback/링크로컬/멀티캐스트/미지정 + CGNAT(100.64/10) 차단."""
    if ip.is_loopback or ip.is_private or ip.is_link_local or ip.is_multicast or ip.is_unspecified or ip.is_reserved:
        return True
    if ip.version == 4 and ip in ipaddress.ip_network("100.64.0.0/10"):
        return True
    return False


def resolve_and_check(host):
    try:
        infos = socket.getaddrinfo(host, None)
    except socket.gaierror as e:
        raise DownloadError(f"DNS 해석 실패: {e}", transient=True)
    if not infos:
        raise DownloadError("DNS 해석 결과 없음", transient=True)
    for info in infos:
        ip = ipaddress.ip_address(info[4][0])
        if is_blocked_ip(ip):
            raise DownloadError("내부망 IP로 해석되는 호스트(SSRF 차단)")


def validate_url(raw_url, cfg):
    u = urllib.parse.urlparse(raw_url)
    if u.scheme not in ("http", "https"):
        raise DownloadError(f"허용되지 않는 스킴: {u.scheme or '(없음)'}")
    if not u.hostname:
        raise DownloadError("호스트 없음")
    if cfg.allow_private_urls:  # 테스트 전용 우회
        return u
    if not host_allowed(u.hostname):
        raise DownloadError("허용되지 않는 호스트")
    resolve_and_check(u.hostname)
    return u


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """리다이렉트를 자동 추적하지 않고 3xx를 그대로 돌려받아 매 hop마다 재검증한다."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _redacted(url):
    """로그/DB error에 남길 URL — query(토큰 가능성)를 제거한다."""
    u = urllib.parse.urlparse(url)
    return f"{u.scheme}://{u.netloc}{u.path}"


def download(raw_url, cfg):
    """(bytes, content_type) 반환. 크기 상한/타임아웃/리다이렉트 제한/allowlist/내부IP 차단."""
    opener = urllib.request.build_opener(_NoRedirect)
    url = raw_url
    for _hop in range(MAX_REDIRECTS + 1):
        u = validate_url(url, cfg)
        headers = {"User-Agent": USER_AGENT}
        # 🚨 기업마당 파일 다운로드는 브라우저 UA + Referer 없으면 403(collector runner 실측과 동일).
        if u.hostname and u.hostname.lower().endswith("bizinfo.go.kr"):
            headers["User-Agent"] = "Mozilla/5.0 (compatible; biz-platform-collector)"
            headers["Referer"] = "https://www.bizinfo.go.kr/"
        req = urllib.request.Request(url, headers=headers, method="GET")
        try:
            resp = opener.open(req, timeout=cfg.download_timeout)
        except urllib.error.HTTPError as e:
            if e.code in (301, 302, 303, 307, 308):
                loc = e.headers.get("Location")
                if not loc:
                    raise DownloadError(f"리다이렉트 Location 없음(HTTP {e.code})")
                url = urllib.parse.urljoin(url, loc)
                continue
            transient = e.code == 429 or 500 <= e.code < 600
            raise DownloadError(f"HTTP {e.code}", transient=transient)
        except urllib.error.URLError as e:
            reason = getattr(e, "reason", e)
            if isinstance(reason, socket.timeout) or "timed out" in str(reason).lower():
                raise DownloadError("다운로드 타임아웃", transient=True)
            raise DownloadError(f"연결 실패: {type(reason).__name__}", transient=True)
        except socket.timeout:
            raise DownloadError("다운로드 타임아웃", transient=True)

        with resp:
            ctype = (resp.headers.get("Content-Type") or "").lower()
            if ctype.startswith("text/html"):
                # 파일 대신 오류/로그인 페이지가 온 경우 — 파서에 넣지 않는다.
                raise DownloadError("HTML 응답(파일 아님)")
            clen = resp.headers.get("Content-Length")
            if clen and clen.isdigit() and int(clen) > cfg.max_download_bytes:
                raise DownloadError(f"파일 크기 초과(Content-Length {clen})")
            chunks, total = [], 0
            while True:
                try:
                    chunk = resp.read(1024 * 256)
                except socket.timeout:
                    raise DownloadError("다운로드 타임아웃(본문)", transient=True)
                if not chunk:
                    break
                total += len(chunk)
                if total > cfg.max_download_bytes:
                    raise DownloadError("파일 크기 초과")
                chunks.append(chunk)
            return b"".join(chunks), ctype
    raise DownloadError("리다이렉트 횟수 초과")


# ---------------------------------------------------------------------------
# 포맷 판정 — 확장자를 믿지 않고 매직바이트로 교차검증(CASE D)
# ---------------------------------------------------------------------------

def magic_kind(body):
    """pdf | zip | ole | None"""
    if body[:4] == b"%PDF":
        return "pdf"
    if len(body) >= 4 and body[:2] == b"PK" and body[2] in (3, 5, 7):
        return "zip"
    if body[:8] == b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1":
        return "ole"
    return None


# 확장자별로 기대하는 컨테이너 시그니처. 불일치면 파서에 넣지 않고 failed.
EXPECTED_MAGIC = {"pdf": "pdf", "hwpx": "zip", "xlsx": "zip", "hwp": "ole", "xls": "ole"}


def normalized_type(file_type, filename):
    ft = (file_type or "").strip().lower().lstrip(".")
    if not ft and filename and "." in filename:
        ft = filename.rsplit(".", 1)[1].lower()
    return ft


# ---------------------------------------------------------------------------
# DB 작업 큐 (attachments 테이블 자체)
# ---------------------------------------------------------------------------

REQUIRED_COLUMNS = ("extraction_attempts", "extraction_started_at", "extraction_completed_at")


def connect(cfg):
    conn = psycopg2.connect(cfg.dsn)
    conn.autocommit = True  # 트랜잭션은 statement 단위로 짧게. 다운로드/파싱 중 열어두지 않는다.
    return conn


def ensure_schema(conn):
    """apiserver 마이그레이션(ensureAttachmentExtractionWorkerColumns)이 먼저 적용돼 있어야
    한다. 없으면 조용히 실패하지 말고 명확히 종료."""
    with conn.cursor() as cur:
        cur.execute(
            "SELECT column_name FROM information_schema.columns WHERE table_name='attachments' AND column_name = ANY(%s)",
            (list(REQUIRED_COLUMNS),),
        )
        have = {r[0] for r in cur.fetchall()}
    missing = [c for c in REQUIRED_COLUMNS if c not in have]
    if missing:
        raise SystemExit(
            f"attachments 컬럼 누락 {missing}: apiserver(ensureAttachmentExtractionWorkerColumns 포함 버전) 마이그레이션을 먼저 적용하세요"
        )


class Scope:
    """어떤 pending 첨부를 대상으로 삼을지. 백필 옵션이 있으면 explicit, 없으면 컷오프 규칙."""

    def __init__(self, attachment_ids=None, notice_id=None, limit=None, created_after=None):
        self.attachment_ids = attachment_ids or []
        self.notice_id = notice_id
        self.limit = limit
        self.created_after = created_after

    @property
    def explicit(self):
        return bool(self.attachment_ids or self.notice_id or self.limit or self.created_after)


def claim_next(conn, cfg, scope):
    """pending 1건을 processing으로 원자 전환해 가져온다. 동시 워커 중복 처리는
    FOR UPDATE SKIP LOCKED로 방지(CASE G). 일시 실패 후 재시도는 extraction_started_at
    (마지막 claim 시각) 기준 backoff."""
    where = [
        "extraction_status = 'pending'",
        "download_status = 'completed'",
        "download_url IS NOT NULL AND download_url <> ''",
        "(extraction_started_at IS NULL OR extraction_started_at < now() - (%s * interval '1 minute'))",
    ]
    params = [cfg.retry_backoff_minutes]
    if scope.attachment_ids:
        where.append("id = ANY(%s::uuid[])")
        params.append(scope.attachment_ids)
    if scope.notice_id:
        where.append("notice_version_id IN (SELECT id FROM notice_versions WHERE notice_id = %s::uuid)")
        params.append(scope.notice_id)
    if scope.created_after:
        where.append("created_at > %s")
        params.append(scope.created_after)
    sql = f"""
        UPDATE attachments a
           SET extraction_status = 'processing',
               extraction_started_at = now(),
               extraction_attempts = extraction_attempts + 1
         WHERE a.id = (
               SELECT id FROM attachments
                WHERE {' AND '.join(where)}
                ORDER BY created_at
                LIMIT 1
                FOR UPDATE SKIP LOCKED)
     RETURNING id, original_filename, file_type, download_url, extraction_attempts, file_size_bytes
    """
    with conn.cursor(cursor_factory=psycopg2.extras.DictCursor) as cur:
        cur.execute(sql, params)
        row = cur.fetchone()
    return dict(row) if row else None


def recover_stale(conn, cfg):
    """processing인 채 stale_minutes 넘게 방치된 행(워커 사망/재배포 kill)을 되돌린다(CASE F).
    attempt 상한을 이미 채웠으면 failed로 종결해 무한 재처리를 막는다."""
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE attachments
               SET extraction_status = CASE WHEN extraction_attempts >= %s THEN 'failed' ELSE 'pending' END,
                   extraction_error  = CASE WHEN extraction_attempts >= %s
                                            THEN 'stale processing: 재시도 상한 도달'
                                            ELSE 'stale processing recovered' END
             WHERE extraction_status = 'processing'
               AND (extraction_started_at IS NULL OR extraction_started_at < now() - (%s * interval '1 minute'))
         RETURNING id, extraction_status
            """,
            (cfg.max_attempts, cfg.max_attempts, cfg.stale_minutes),
        )
        rows = cur.fetchall()
    for att_id, status in rows:
        logger.warning("stale extraction recovered attachment=%s -> %s", att_id, status)
    return len(rows)


def finish(conn, att_id, status, text=None, error=None):
    """종결 상태 저장 — 한 statement. completed면 extracted_text/completed_at 채우고 error NULL."""
    if error:
        error = error[:500]
    with conn.cursor() as cur:
        if status == "completed":
            cur.execute(
                """UPDATE attachments SET extraction_status='completed', extracted_text=%s, extraction_error=NULL,
                          extraction_completed_at=now() WHERE id=%s AND extraction_status='processing'""",
                (text, att_id),
            )
        else:
            cur.execute(
                "UPDATE attachments SET extraction_status=%s, extraction_error=%s WHERE id=%s AND extraction_status='processing'",
                (status, error, att_id),
            )
        return cur.rowcount


def requeue_transient(conn, att_id, error):
    """일시 실패: pending으로 되돌리되 extraction_started_at은 유지 → claim의 backoff 조건이
    retry_backoff_minutes 뒤에야 다시 집도록 한다(같은 건 hot loop 방지)."""
    with conn.cursor() as cur:
        cur.execute(
            "UPDATE attachments SET extraction_status='pending', extraction_error=%s WHERE id=%s AND extraction_status='processing'",
            (error[:500], att_id),
        )


# ---------------------------------------------------------------------------
# 한 건 처리
# ---------------------------------------------------------------------------

class ParseTimeout(ExtractionError):
    pass


def _alarm_handler(signum, frame):
    raise ParseTimeout("파싱 시간 초과")


def extract_bytes(body, ftype, parse_timeout=0):
    """임시파일에 쓰고 run_extraction 파서 호출. 파서는 경로 기반이라 /tmp 임시파일 사용,
    끝나면 즉시 삭제(로컬 파일 공유 없음). parse_timeout>0이면 SIGALRM으로 상한을 둬
    비정상 PDF 하나가 워커 전체를 영원히 멈추지 못하게 한다(hwp5txt는 자체 60초 상한)."""
    with tempfile.NamedTemporaryFile(prefix="att-", suffix="." + ftype, delete=False) as tf:
        tf.write(body)
        path = tf.name
    prev = None
    try:
        if parse_timeout > 0 and hasattr(signal, "SIGALRM"):
            prev = signal.signal(signal.SIGALRM, _alarm_handler)
            signal.alarm(parse_timeout)
        text = EXTRACTORS[ftype](path)
        return text.replace("\x00", "")
    finally:
        if prev is not None:
            signal.alarm(0)
            signal.signal(signal.SIGALRM, prev)
        try:
            os.remove(path)
        except OSError:
            pass


def process_one(conn, cfg, att):
    att_id = str(att["id"])
    ftype = normalized_type(att.get("file_type"), att.get("original_filename"))
    attempt = att.get("extraction_attempts")
    logger.info("attachment extraction claimed attachment=%s type=%s attempt=%s size=%s",
                att_id, ftype or "(none)", attempt, att.get("file_size_bytes"))
    t0 = time.monotonic()

    if ftype not in SUPPORTED_TYPES:
        finish(conn, att_id, "unsupported", error=f"미지원 형식: {ftype or '(확장자 없음)'}")
        logger.info("attachment extraction unsupported attachment=%s type=%s", att_id, ftype or "(none)")
        return "unsupported"

    try:
        body, _ctype = download(att["download_url"], cfg)
    except DownloadError as e:
        msg = f"다운로드 실패: {e}"
        if e.transient and attempt < cfg.max_attempts:
            requeue_transient(conn, att_id, msg)
            logger.warning("attachment extraction retry-later attachment=%s attempt=%s/%s reason=%s host=%s",
                           att_id, attempt, cfg.max_attempts, e, _redacted(att["download_url"]))
            return "retry"
        finish(conn, att_id, "failed", error=msg)
        logger.warning("attachment extraction failed attachment=%s reason=%s", att_id, e)
        return "failed"

    kind = magic_kind(body)
    if EXPECTED_MAGIC.get(ftype) != kind:
        finish(conn, att_id, "failed",
               error=f"형식 불일치: 확장자 {ftype}, 실제 시그니처 {kind or '알 수 없음'} ({len(body)}바이트)")
        logger.warning("attachment extraction failed attachment=%s reason=magic-mismatch ext=%s magic=%s",
                       att_id, ftype, kind)
        return "failed"

    try:
        text = extract_bytes(body, ftype, cfg.parse_timeout)
    except ExtractionError as e:
        # 파서가 명시적으로 거부(스캔 이미지 PDF·빈 문서·hwp5txt 실패 등) — 영구 오류, 재시도 안 함.
        finish(conn, att_id, "failed", error=f"{type(e).__name__}: {e}")
        logger.warning("attachment extraction failed attachment=%s reason=%s", att_id, str(e)[:200])
        return "failed"
    except Exception as e:  # noqa: broad-except — 라이브러리 예외 종류가 다양하다. 영구 오류로 취급.
        finish(conn, att_id, "failed", error=f"{type(e).__name__}: {str(e)[:200]}")
        logger.warning("attachment extraction failed attachment=%s reason=%s: %s", att_id, type(e).__name__, str(e)[:200])
        return "failed"

    if not text.strip():
        finish(conn, att_id, "failed", error="추출 결과가 비어 있음")
        logger.warning("attachment extraction failed attachment=%s reason=empty-text", att_id)
        return "failed"

    n = finish(conn, att_id, "completed", text=text)
    if n == 0:
        # 우리가 processing으로 잡은 행이 그 사이 다른 상태로 바뀜(stale 복구 등) — 덮어쓰지 않는다.
        logger.warning("attachment extraction completed but row state changed attachment=%s (not saved)", att_id)
        return "skipped"
    logger.info("attachment extraction completed attachment=%s type=%s chars=%d elapsed=%.1fs",
                att_id, ftype, len(text), time.monotonic() - t0)
    return "completed"


# ---------------------------------------------------------------------------
# 루프
# ---------------------------------------------------------------------------

class Worker:
    def __init__(self, cfg, scope, once=False, mode="daemon"):
        self.cfg = cfg
        self.scope = scope
        self.once = once
        self.mode = mode
        self.stopping = False
        self.counts = {"completed": 0, "failed": 0, "unsupported": 0, "retry": 0, "skipped": 0}

    def request_stop(self, signum, _frame):
        logger.info("signal %s received; finishing current item then exiting", signum)
        self.stopping = True

    def run(self):
        signal.signal(signal.SIGTERM, self.request_stop)
        signal.signal(signal.SIGINT, self.request_stop)
        conn = connect(self.cfg)
        ensure_schema(conn)
        logger.info("worker start mode=%s once=%s created_after=%s poll=%ss max_attempts=%s stale=%smin",
                    self.mode, self.once, self.scope.created_after, self.cfg.poll_seconds,
                    self.cfg.max_attempts, self.cfg.stale_minutes)
        processed = 0
        last_stale_check = 0.0
        try:
            while not self.stopping:
                now = time.monotonic()
                if now - last_stale_check >= 60:
                    recover_stale(conn, self.cfg)
                    last_stale_check = now
                if self.scope.limit and processed >= self.scope.limit:
                    break
                try:
                    att = claim_next(conn, self.cfg, self.scope)
                except psycopg2.OperationalError as e:
                    logger.error("db error on claim: %s; reconnecting", str(e)[:200])
                    time.sleep(self.cfg.poll_seconds)
                    try:
                        conn.close()
                    except Exception:  # noqa: broad-except
                        pass
                    conn = connect(self.cfg)
                    continue
                if att is None:
                    if self.once:
                        break
                    time.sleep(self.cfg.poll_seconds)
                    continue
                result = process_one(conn, self.cfg, att)
                self.counts[result] = self.counts.get(result, 0) + 1
                processed += 1
        finally:
            try:
                conn.close()
            except Exception:  # noqa: broad-except
                pass
        logger.info("worker exit processed=%d %s", processed, self.counts)
        return self.counts


def parse_args(argv=None):
    p = argparse.ArgumentParser(description="첨부파일 텍스트 추출 워커")
    p.add_argument("--once", action="store_true", help="대기열이 빌 때까지 1회 처리 후 종료")
    p.add_argument("--attachment-id", action="append", default=[], help="특정 첨부만(반복 가능) — 백필")
    p.add_argument("--notice-id", default=None, help="특정 공고의 첨부만 — 백필")
    p.add_argument("--limit", type=int, default=None, help="최대 처리 건수 — 백필")
    p.add_argument("--created-after", default=None, help="이 시각(ISO8601) 이후 생성된 첨부만 — 백필")
    return p.parse_args(argv)


def build_scope(args, cfg, started_at):
    """(scope, once, mode). 백필 옵션이 있으면 backfill(1회, 컷오프 무시); 아니면
    EXTRACTOR_PROCESS_EXISTING에 따라 전체 또는 '시작 시각(또는 EXTRACTOR_CREATED_AFTER) 이후 새 첨부만'."""
    scope = Scope(args.attachment_id, args.notice_id, args.limit, args.created_after)
    if scope.explicit:
        return scope, True, "backfill"
    if cfg.process_existing:
        return Scope(), args.once, "all-pending"
    return Scope(created_after=cfg.created_after or started_at.isoformat()), args.once, "new-only"


def main(argv=None):
    args = parse_args(argv)
    cfg = Config()
    if not cfg.dsn:
        logger.error("DATABASE_URL 환경변수가 설정되어 있지 않습니다")
        return 1
    if cfg.allow_private_urls:
        logger.warning("EXTRACTOR_ALLOW_PRIVATE_URLS=true — 테스트 전용 설정입니다. 운영에서 켜지 마세요")
    started_at = datetime.now(timezone.utc)
    scope, once, mode = build_scope(args, cfg, started_at)
    Worker(cfg, scope, once=once, mode=mode).run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
