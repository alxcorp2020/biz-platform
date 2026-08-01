#!/usr/bin/env python3
"""규칙(정규식/패턴 매칭) 기반 자격조건/제출서류 구조화 추출 — 1차 버전.

Phase 4(서류자동화 고도화)부터 이 로직은 Go로 포팅돼 apiserver 백그라운드
배치(collector/internal/api/document_extraction.go)가 1시간마다 자동
실행한다 — 이 스크립트를 수동으로 또 돌리면 워터마크 컬럼
(attachments.section_extraction_processed_at)을 모르는 채로 중복 작업할
뿐이니, 특정 첨부파일 1건 디버깅(--attachment-id) 용도가 아니면 쓰지 말 것.

attachments.extracted_text(extraction_status='completed')에서 "참가자격",
"제출서류" 류의 절/섹션을 느슨한 패턴으로 찾아
eligibility_conditions/required_documents에 저장한다. AI 연동은 다음 단계 —
여기서는 규칙만 사용한다.

세부 분류(지역/업력/면허 등)는 하지 않는다 — category는 전부 'general'로
통일해서 저장하고, 원문(condition_name/source_text)을 그대로 남겨 나중에
세분화할 수 있게 한다.

원칙(스펙): 원문 근거 없는 추출 금지 — 모든 행은 source_attachment_id로
어느 첨부파일에서 뽑았는지 연결되고, source_text에 실제 원문이 남는다.
목록이 없으면("참조"만 있으면) 목록을 추측해서 만들어내지 않는다.

사용법:
    venv/bin/python extract_sections.py [--limit N]
"""
import argparse
import logging
import os
import re

import psycopg2

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger("extract_sections")

ELIGIBILITY_KEYWORDS = ["참가자격", "응모자격", "응찰자격", "자격요건", "참가요건"]
DOCUMENT_KEYWORDS = ["제출서류", "구비서류", "첨부서류"]

REFERENCE_ONLY_MARKERS = ["참조", "별첨", "별지 서식", "별지서식", "붙임"]

RULE_ENGINE_VERSION = "rule-sections-v1"
CONFIDENCE = 0.70
TABLE_LOOKAHEAD = 5  # 섹션 끝에서 이만큼 더 내다보며 표 인접 여부를 판단

# 섹션 경계 판단: 짧은 줄(<=30자)이면서 숫자/기호로 시작하거나 문장
# 종결어미로 끝나지 않는(=명사형으로 끝나는) 경우를 "헤더처럼 보이는 줄"로 본다.
# 완벽한 판별이 아니라 의도적으로 느슨한 휴리스틱이다.
_HEADER_PREFIX_RE = re.compile(
    r'^([0-9]+[.\)]|[①-⑩]|[가나다라마바사아자차카타파하][.\)]|[○◦●■□❍▶◆★☆※\-\*])'
)
_SENTENCE_ENDING_RE = re.compile(r'(다|함|음|까|요|임|됨|습니다|입니다)[.]?$')


_PLACEHOLDER_TOKENS = ("<표>", "<그림>", "<이미지>")


def looks_like_header(line):
    s = line.strip()
    if not s or len(s) > 30:
        return False
    if s in _PLACEHOLDER_TOKENS:
        return False  # 표/그림 플레이스홀더를 섹션 종료 신호로 오인하면 안 됨
    if _HEADER_PREFIX_RE.match(s):
        return True
    return not _SENTENCE_ENDING_RE.search(s)


def _strip_list_prefix(line):
    return _HEADER_PREFIX_RE.sub("", line.strip(), count=1).strip(" .)")


def extract_sections(text, keywords):
    """text에서 keywords 중 하나라도 (띄어쓰기 무관하게) 포함된 줄을 앵커로
    찾아 섹션을 잘라낸다. 앵커 자체가 헤더처럼 보이면 다음 헤더 전까지,
    아니면(본문에 묻힌 한 줄짜리 언급이면) 근처 몇 줄만 좁게 본다 — 안 그러면
    무관한 다음 헤더까지 수십 줄을 잘못 삼킬 수 있다."""
    lines = text.splitlines()
    normalized = ["".join(l.split()) for l in lines]

    hit_indices = [i for i, norm in enumerate(normalized) if any(kw in norm for kw in keywords)]

    sections = []
    for idx in hit_indices:
        anchor_text = lines[idx].strip()
        is_header = looks_like_header(anchor_text)
        start = idx + 1 if is_header else idx
        search_limit = len(lines) if is_header else min(len(lines), idx + 40)

        end = search_limit
        for j in range(idx + 1, search_limit):
            if looks_like_header(lines[j]):
                end = j
                break

        section_text = "\n".join(lines[start:end]).strip()

        # 표는 섹션 안이 아니라 바로 뒤에 이어지는 경우도 많다(헤더 다음
        # 문장 몇 줄 뒤에 표가 나오는 식) — 섹션 끝에서 몇 줄 더 내다본다.
        lookahead_end = min(len(lines), end + TABLE_LOOKAHEAD)
        table_in_section = any(t in section_text or t in anchor_text for t in _PLACEHOLDER_TOKENS)
        table_after_section = any(lines[k].strip() in _PLACEHOLDER_TOKENS for k in range(end, lookahead_end))
        table_nearby = table_in_section or table_after_section

        sections.append({
            "anchor_line_no": idx,
            "anchor_text": anchor_text,
            "is_header": is_header,
            "section_text": section_text,
            "has_table_nearby": table_nearby,
        })
    return sections


def is_reference_only(section_text, list_like_lines):
    if len(list_like_lines) >= 2:
        return False
    if not section_text:
        return True
    return any(marker in section_text for marker in REFERENCE_ONLY_MARKERS)


_DOCNAME_MAX_LEN = 60


def looks_like_document_name(line_after_prefix):
    """실제 서류명은 대체로 짧은 명사구다("사업자등록증 사본 1부",
    "직접생산확인증명서 1부"). 노이즈로 섞여 들어오는 건 대부분 유의사항/경고
    문장이라 길고 "~합니다/~바랍니다/~습니다" 같은 문장 종결어미로 끝난다."""
    s = line_after_prefix.strip()
    if not s or len(s) > _DOCNAME_MAX_LEN:
        return False
    return not _SENTENCE_ENDING_RE.search(s)


def find_list_like_lines(section_text):
    lines = [l.strip() for l in section_text.splitlines() if l.strip()]
    candidates = [l for l in lines if _HEADER_PREFIX_RE.match(l) and l not in ("<표>",)]
    return [l for l in candidates if looks_like_document_name(_strip_list_prefix(l))]


def review_status_for(has_table_nearby):
    return "review_required" if has_table_nearby else "pending"


def build_eligibility_rows(sections, attachment_id):
    rows = []
    for sec in sections:
        source_text = (sec["anchor_text"] + "\n" + sec["section_text"]).strip()
        rows.append({
            "category": "general",
            "condition_name": sec["anchor_text"][:200],
            "operator": "n/a",
            "threshold_value": None,
            "source_text": source_text,
            "source_attachment_id": attachment_id,
            "confidence": CONFIDENCE,
            "review_status": review_status_for(sec["has_table_nearby"]),
        })
    return rows


def build_required_document_rows(sections, attachment_id):
    rows = []
    for sec in sections:
        section_text = sec["section_text"]
        list_like = find_list_like_lines(section_text)
        review_status = review_status_for(sec["has_table_nearby"])

        if is_reference_only(section_text, list_like):
            rows.append({
                "document_name": "(본문에 목록 없음 - 별첨/타 문서 참조)",
                "source_text": (sec["anchor_text"] + "\n" + section_text).strip(),
                "source_attachment_id": attachment_id,
                "confidence": CONFIDENCE,
                "review_status": review_status,
            })
        elif list_like:
            for line in list_like:
                rows.append({
                    "document_name": _strip_list_prefix(line)[:500] or line[:500],
                    "source_text": line,
                    "source_attachment_id": attachment_id,
                    "confidence": CONFIDENCE,
                    "review_status": review_status,
                })
        elif section_text:
            rows.append({
                "document_name": section_text[:80].strip() + ("..." if len(section_text) > 80 else ""),
                "source_text": (sec["anchor_text"] + "\n" + section_text).strip(),
                "source_attachment_id": attachment_id,
                "confidence": CONFIDENCE,
                "review_status": review_status,
            })
        # section_text가 완전히 비어있으면(앵커 줄 자체가 전부인 경우) 아무것도
        # 만들어내지 않는다 — 목록을 추측하지 않는다는 원칙.
    return rows


def resolve_notice_version_id(cur, attachment_id):
    cur.execute("SELECT notice_version_id FROM attachments WHERE id = %s", (attachment_id,))
    row = cur.fetchone()
    return row[0] if row else None


def process_attachment(conn, att_id, text):
    with conn.cursor() as cur:
        notice_version_id = resolve_notice_version_id(cur, att_id)
        if notice_version_id is None:
            logger.warning("attachment %s: notice_version_id 없음, 스킵", att_id)
            return 0, 0

        elig_sections = extract_sections(text, ELIGIBILITY_KEYWORDS)
        doc_sections = extract_sections(text, DOCUMENT_KEYWORDS)
        elig_rows = build_eligibility_rows(elig_sections, att_id)
        doc_rows = build_required_document_rows(doc_sections, att_id)

        # 재실행해도 중복이 쌓이지 않도록, 이 첨부파일 기준으로 만든 규칙
        # 기반(category='general') 행을 지우고 다시 넣는다.
        cur.execute(
            "DELETE FROM eligibility_conditions WHERE source_attachment_id = %s AND category = 'general'",
            (att_id,),
        )
        cur.execute("DELETE FROM required_documents WHERE source_attachment_id = %s", (att_id,))

        for r in elig_rows:
            cur.execute(
                """INSERT INTO eligibility_conditions
                       (notice_version_id, category, condition_name, operator, threshold_value,
                        source_text, source_attachment_id, confidence, review_status)
                   VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s)""",
                (notice_version_id, r["category"], r["condition_name"], r["operator"], r["threshold_value"],
                 r["source_text"], r["source_attachment_id"], r["confidence"], r["review_status"]),
            )
        for r in doc_rows:
            cur.execute(
                """INSERT INTO required_documents
                       (notice_version_id, document_name, source_text,
                        source_attachment_id, confidence, review_status)
                   VALUES (%s,%s,%s,%s,%s,%s)""",
                (notice_version_id, r["document_name"], r["source_text"],
                 r["source_attachment_id"], r["confidence"], r["review_status"]),
            )
    conn.commit()
    return len(elig_rows), len(doc_rows)


def main():
    parser = argparse.ArgumentParser(description="자격조건/제출서류 규칙 기반 구조화 추출")
    parser.add_argument("--limit", type=int, default=None)
    parser.add_argument("--attachment-id", default=None, help="특정 첨부파일 1건만 처리(디버깅용)")
    args = parser.parse_args()

    dsn = os.environ.get("DATABASE_URL")
    if not dsn:
        logger.error("DATABASE_URL 환경변수가 설정되어 있지 않습니다")
        raise SystemExit(1)

    conn = psycopg2.connect(dsn)

    query = "SELECT id, extracted_text FROM attachments WHERE extraction_status = 'completed'"
    params = []
    if args.attachment_id:
        query += " AND id = %s"
        params.append(args.attachment_id)
    query += " ORDER BY created_at"
    if args.limit:
        query += " LIMIT %s"
        params.append(args.limit)

    with conn.cursor() as cur:
        cur.execute(query, params)
        rows = cur.fetchall()

    logger.info("처리 대상: %d건", len(rows))

    total_elig, total_doc, docs_with_hits = 0, 0, 0
    for att_id, text in rows:
        if not text:
            continue
        n_elig, n_doc = process_attachment(conn, att_id, text)
        if n_elig or n_doc:
            docs_with_hits += 1
            logger.info("attachment %s: 자격조건 %d건, 제출서류 %d건", att_id, n_elig, n_doc)
        total_elig += n_elig
        total_doc += n_doc

    conn.close()
    logger.info("종료: 매칭 문서 %d/%d, 자격조건 %d건, 제출서류 %d건",
                docs_with_hits, len(rows), total_elig, total_doc)


if __name__ == "__main__":
    main()
