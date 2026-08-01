#!/usr/bin/env python3
"""규칙 기반 추출(1차)을 보완하는 AI 기반 추출(2차) — Claude API 사용.

Phase 4(서류자동화 고도화)부터 이 로직은 Go로 포팅돼 apiserver 백그라운드
배치(collector/internal/api/document_extraction.go)가 1시간마다 자동
실행한다 — 이 스크립트를 수동으로 또 돌리면 워터마크 컬럼
(eligibility_conditions/required_documents.ai_supplement_attempted_at)을
모르는 채로 같은 항목을 또 호출해 비용이 중복 발생하니, 자동 배치 자체를
디버깅할 때가 아니면 쓰지 말 것.

비용 절감 원칙(스펙 "공고 공통분석은 한 번만" 재사용): 이미 confidence 높게
뽑힌 규칙 기반 행은 다시 건드리지 않는다. eligibility_conditions/
required_documents 중 review_status='review_required'인 행만 대상으로 한다
(1차 규칙 기반이 표 근처 등 불확실하다고 스스로 표시한 것만).

환각 방지(스펙 원칙): AI가 반환한 각 항목의 quoted_text가 실제로 원문(공백
정규화 기준)에 존재하는지 검증하고, 검증 통과한 것만 저장한다. 검증 실패
항목은 버린다 — 목록을 추측해서 만들어내지 않는다는 1차 규칙 기반의 원칙을
그대로 따른다.

저장 규칙: confidence=0.6(규칙 기반의 0.70보다 낮음), review_status='pending'
(AI 결과도 최종 확정 아님 — 사람 검토 대상), extraction_method='ai',
model_version에 사용 모델명 기록(재현성 확인용).

사용법:
    venv/bin/python ai_extract.py --estimate            # 비용 추정만 (API 추출 호출 없음)
    venv/bin/python ai_extract.py --limit 10             # 소규모 테스트 실행
    venv/bin/python ai_extract.py --all --confirm-full-run   # 전체 review_required 처리 (승인 필요)

환경변수:
    DATABASE_URL, ANTHROPIC_API_KEY
"""
import argparse
import logging
import os
import re

import anthropic
import psycopg2

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger("ai_extract")

MODEL = "claude-sonnet-5"
AI_CONFIDENCE = 0.60

# claude-sonnet-5 가격: 2026-08-31까지 인트로 단가, 이후 정가로 복귀.
INPUT_PRICE_INTRO = 2.00
OUTPUT_PRICE_INTRO = 10.00
INPUT_PRICE_STANDARD = 3.00
OUTPUT_PRICE_STANDARD = 15.00

# 실제 응답을 받기 전까지는 출력 토큰을 알 수 없어, 견적 단계에서는 건당
# 이 정도로 가정한다(짧은 항목 1~2개 + JSON 구조 오버헤드). 소규모 테스트
# 실행 후에는 실측 평균으로 대체한 견적을 다시 출력한다.
ASSUMED_OUTPUT_TOKENS_PER_ROW = 200

# 앵커(원문 내 첫 줄) 위치부터 이만큼을 원문에서 잘라 AI에게 넘긴다 —
# 1차 규칙 기반이 review_required로 표시한 이유(표 근처 등)의 실제 내용을
# 포함시키기 위해, 저장된 source_text보다 넓게 잡는다.
CONTEXT_WINDOW_CHARS = 3000

ELIGIBILITY_TOOL = {
    "name": "extract_eligibility_conditions",
    "description": "원문에서 실제로 발견되는 참가자격 조건만 추출합니다. 원문에 없는 내용은 절대 만들지 마세요.",
    "strict": True,
    "input_schema": {
        "type": "object",
        "properties": {
            "conditions": {
                "type": "array",
                "items": {
                    "type": "object",
                    "properties": {
                        "condition_name": {
                            "type": "string",
                            "description": "조건을 요약하는 짧은 제목 (200자 이내)",
                        },
                        "quoted_text": {
                            "type": "string",
                            "description": "아래 원문에서 그대로 복사한 문장/구절. 절대 새로 만들거나 바꿔쓰지 말 것.",
                        },
                    },
                    "required": ["condition_name", "quoted_text"],
                    "additionalProperties": False,
                },
            },
        },
        "required": ["conditions"],
        "additionalProperties": False,
    },
}

DOCUMENT_TOOL = {
    "name": "extract_required_documents",
    "description": "원문에서 실제로 발견되는 제출서류 항목만 추출합니다. 원문에 없는 내용은 절대 만들지 마세요.",
    "strict": True,
    "input_schema": {
        "type": "object",
        "properties": {
            "documents": {
                "type": "array",
                "items": {
                    "type": "object",
                    "properties": {
                        "document_name": {
                            "type": "string",
                            "description": "서류명 (500자 이내)",
                        },
                        "quoted_text": {
                            "type": "string",
                            "description": "아래 원문에서 그대로 복사한 문장/구절. 절대 새로 만들거나 바꿔쓰지 말 것.",
                        },
                    },
                    "required": ["document_name", "quoted_text"],
                    "additionalProperties": False,
                },
            },
        },
        "required": ["documents"],
        "additionalProperties": False,
    },
}

PROMPT_TEMPLATE = {
    "eligibility": (
        "다음은 대한민국 공공입찰 공고문에서 발췌한 원문입니다. '<표>'는 표가 있던 자리를 나타내는 "
        "마커이고, 그 아래 줄들은 표의 각 행을 '|'로 구분해 풀어 쓴 것입니다.\n\n"
        "이 원문에서 실제 참가자격 요건 항목만 추출하세요.\n\n"
        "중요한 규칙:\n"
        "- 원문에 없는 내용은 절대 만들어내지 마세요.\n"
        "- quoted_text는 아래 원문에 실제로 등장하는 문장/구절을 그대로 복사해야 합니다. 요약하거나 바꿔 쓰지 마세요.\n"
        "- 참가자격 요건이 아닌 일반 안내문, 유의사항, 서류 목록은 포함하지 마세요.\n"
        "- 항목이 없으면 빈 배열을 반환하세요.\n\n"
        "원문:\n---\n{context}\n---"
    ),
    "document": (
        "다음은 대한민국 공공입찰 공고문에서 발췌한 원문입니다. '<표>'는 표가 있던 자리를 나타내는 "
        "마커이고, 그 아래 줄들은 표의 각 행을 '|'로 구분해 풀어 쓴 것입니다.\n\n"
        "이 원문에서 실제 제출서류 항목만 추출하세요.\n\n"
        "중요한 규칙:\n"
        "- 원문에 없는 내용은 절대 만들어내지 마세요.\n"
        "- quoted_text는 아래 원문에 실제로 등장하는 문장/구절을 그대로 복사해야 합니다. 요약하거나 바꿔 쓰지 마세요.\n"
        "- 서류 항목이 아닌 일반 안내문, 유의사항, 참가자격 조건은 포함하지 마세요.\n"
        "- 항목이 없으면 빈 배열을 반환하세요.\n\n"
        "원문:\n---\n{context}\n---"
    ),
}


def fetch_eligibility_targets(cur, limit=None):
    query = """
        SELECT ec.id, ec.notice_version_id, ec.source_attachment_id, ec.source_text, a.extracted_text
        FROM eligibility_conditions ec
        JOIN attachments a ON a.id = ec.source_attachment_id
        WHERE ec.review_status = 'review_required' AND ec.source_attachment_id IS NOT NULL
        ORDER BY ec.created_at
    """
    if limit:
        query += f" LIMIT {int(limit)}"
    cur.execute(query)
    return [
        {
            "id": r[0], "notice_version_id": r[1], "source_attachment_id": r[2],
            "source_text": r[3], "extracted_text": r[4],
        }
        for r in cur.fetchall()
    ]


def fetch_document_targets(cur, limit=None):
    query = """
        SELECT rd.id, rd.notice_version_id, rd.source_attachment_id, rd.source_text, a.extracted_text
        FROM required_documents rd
        JOIN attachments a ON a.id = rd.source_attachment_id
        WHERE rd.review_status = 'review_required' AND rd.source_attachment_id IS NOT NULL
        ORDER BY rd.id
    """
    if limit:
        query += f" LIMIT {int(limit)}"
    cur.execute(query)
    return [
        {
            "id": r[0], "notice_version_id": r[1], "source_attachment_id": r[2],
            "source_text": r[3], "extracted_text": r[4],
        }
        for r in cur.fetchall()
    ]


def first_line(text):
    for line in (text or "").splitlines():
        line = line.strip()
        if line:
            return line
    return ""


def build_context(extracted_text, source_text):
    anchor = first_line(source_text)
    if extracted_text and anchor:
        idx = extracted_text.find(anchor)
        if idx != -1:
            return extracted_text[idx: idx + CONTEXT_WINDOW_CHARS]
    return source_text or ""


_WS_RE = re.compile(r"\s+")


def normalize_ws(s):
    return _WS_RE.sub(" ", s or "").strip()


def verify_and_locate(quoted_text, context):
    """quoted_text가 context 안에 실제로 존재하는지 확인(공백/줄바꿈 차이는
    허용, 내용 자체의 존재 여부만 검증). 통과하면 원본 context에서 그
    구간을 다시 찾아 원본 그대로(모델이 옮겨적으며 생긴 사소한 표기 차이
    없이) 반환한다. 존재하지 않으면 None — 이 항목은 저장하지 않는다."""
    norm_quote = normalize_ws(quoted_text)
    if not norm_quote:
        return None
    if norm_quote not in normalize_ws(context):
        return None
    tokens = [t for t in norm_quote.split(" ") if t]
    pattern = re.compile(r"\s+".join(re.escape(t) for t in tokens))
    m = pattern.search(context)
    return m.group(0) if m else quoted_text


def build_message(kind, context):
    return [{"role": "user", "content": PROMPT_TEMPLATE[kind].format(context=context)}]


def count_tokens(client, kind, context):
    tool = ELIGIBILITY_TOOL if kind == "eligibility" else DOCUMENT_TOOL
    result = client.messages.count_tokens(
        model=MODEL, tools=[tool], messages=build_message(kind, context),
    )
    return result.input_tokens


def call_claude(client, kind, context):
    tool = ELIGIBILITY_TOOL if kind == "eligibility" else DOCUMENT_TOOL
    response = client.messages.create(
        model=MODEL,
        max_tokens=2048,
        thinking={"type": "disabled"},
        output_config={"effort": "low"},
        tools=[tool],
        tool_choice={"type": "tool", "name": tool["name"]},
        messages=build_message(kind, context),
    )
    key = "conditions" if kind == "eligibility" else "documents"
    items = []
    for block in response.content:
        if block.type == "tool_use":
            items = block.input.get(key, [])
    return items, response.usage


def save_eligibility_items(cur, row, items):
    for it in items:
        cur.execute(
            """INSERT INTO eligibility_conditions
                   (notice_version_id, category, condition_name, operator, threshold_value,
                    source_text, source_attachment_id, confidence, review_status,
                    extraction_method, model_version)
               VALUES (%s,'general',%s,'n/a',NULL,%s,%s,%s,'pending','ai',%s)""",
            (row["notice_version_id"], it["condition_name"][:200], it["quoted_text"],
             row["source_attachment_id"], AI_CONFIDENCE, MODEL),
        )


def save_document_items(cur, row, items):
    for it in items:
        cur.execute(
            """INSERT INTO required_documents
                   (notice_version_id, document_name, source_text,
                    source_attachment_id, confidence, review_status,
                    extraction_method, model_version)
               VALUES (%s,%s,%s,%s,%s,'pending','ai',%s)""",
            (row["notice_version_id"], it["document_name"][:500], it["quoted_text"],
             row["source_attachment_id"], AI_CONFIDENCE, MODEL),
        )


def print_cost_report(label, row_count, input_tokens, output_tokens):
    intro_cost = (input_tokens / 1_000_000 * INPUT_PRICE_INTRO) + (output_tokens / 1_000_000 * OUTPUT_PRICE_INTRO)
    standard_cost = (input_tokens / 1_000_000 * INPUT_PRICE_STANDARD) + (output_tokens / 1_000_000 * OUTPUT_PRICE_STANDARD)
    logger.info("[%s] 대상 %d건", label, row_count)
    logger.info("  입력 토큰: %s", f"{input_tokens:,}")
    logger.info("  출력 토큰: %s", f"{output_tokens:,}")
    logger.info("  예상 비용 (claude-sonnet-5, 2026-08-31까지 인트로 단가 $2/$10 per MTok): $%.4f", intro_cost)
    logger.info("  예상 비용 (인트로 종료 후 정가 $3/$15 per MTok): $%.4f", standard_cost)


def estimate_mode(client, cur):
    elig_rows = fetch_eligibility_targets(cur)
    doc_rows = fetch_document_targets(cur)
    logger.info("review_required 전체: 자격조건 %d건, 제출서류 %d건", len(elig_rows), len(doc_rows))

    total_input = 0
    for kind, rows in (("eligibility", elig_rows), ("document", doc_rows)):
        for i, row in enumerate(rows, 1):
            context = build_context(row["extracted_text"], row["source_text"])
            total_input += count_tokens(client, kind, context)
            if i % 50 == 0:
                logger.info("  토큰 계산 진행: %s %d/%d", kind, i, len(rows))

    total_rows = len(elig_rows) + len(doc_rows)
    assumed_output = total_rows * ASSUMED_OUTPUT_TOKENS_PER_ROW
    print_cost_report("전체 review_required (출력 토큰은 건당 %d로 가정, 실측 아님)" % ASSUMED_OUTPUT_TOKENS_PER_ROW,
                       total_rows, total_input, assumed_output)


def run_mode(client, conn, cur, limit):
    if limit is None:
        elig_rows = fetch_eligibility_targets(cur)
        doc_rows = fetch_document_targets(cur)
    else:
        half = max(1, limit // 2)
        elig_rows = fetch_eligibility_targets(cur, limit=half)
        doc_rows = fetch_document_targets(cur, limit=limit - half)

    logger.info("처리 대상: 자격조건 %d건, 제출서류 %d건", len(elig_rows), len(doc_rows))

    total_verified, total_discarded = 0, 0
    total_input_tokens, total_output_tokens = 0, 0

    for kind, rows, saver in (
        ("eligibility", elig_rows, save_eligibility_items),
        ("document", doc_rows, save_document_items),
    ):
        for row in rows:
            context = build_context(row["extracted_text"], row["source_text"])
            items, usage = call_claude(client, kind, context)
            total_input_tokens += usage.input_tokens
            total_output_tokens += usage.output_tokens

            verified, discarded = [], []
            for it in items:
                if not isinstance(it, dict):
                    continue
                located = verify_and_locate(it.get("quoted_text", ""), context)
                if located is None:
                    discarded.append(it)
                    continue
                verified.append({**it, "quoted_text": located})

            saver(cur, row, verified)
            conn.commit()

            total_verified += len(verified)
            total_discarded += len(discarded)

            name_field = "condition_name" if kind == "eligibility" else "document_name"
            logger.info("[%s] %s: 모델 반환 %d건 -> 검증 통과 %d건, 검증 실패(원문에 없어 버림) %d건",
                        kind, row["id"], len(items), len(verified), len(discarded))
            for it in verified:
                logger.info("    [저장] %s: %s", it[name_field][:60], it["quoted_text"][:80].replace("\n", " "))
            for it in discarded:
                logger.info("    [버림-검증실패] %s: %s", it.get(name_field, "")[:60],
                            it.get("quoted_text", "")[:80].replace("\n", " "))

    n_rows = len(elig_rows) + len(doc_rows)
    logger.info("종료: 처리 %d건, 검증 통과(저장) %d건, 검증 실패(버림) %d건",
                n_rows, total_verified, total_discarded)
    logger.info("실측 토큰: 입력 %s, 출력 %s", f"{total_input_tokens:,}", f"{total_output_tokens:,}")

    if n_rows:
        avg_output = total_output_tokens / n_rows
        full_scope_estimate_rows = 299  # 이번 세션 기준 review_required 전체 건수(수시로 변함 — 참고용)
        logger.info(
            "참고: 이번 배치의 건당 평균 출력 토큰은 %.0f — 전체(%d건 안팎) 실행 시 견적을 다시 계산하려면 "
            "--estimate로 실측 입력 토큰을 구하고, 출력 토큰은 %.0f * 건수로 대체 추정 가능",
            avg_output, full_scope_estimate_rows, avg_output,
        )


def main():
    parser = argparse.ArgumentParser(description="review_required 항목 AI 보완 추출 (2차, claude-sonnet-5)")
    parser.add_argument("--estimate", action="store_true", help="비용 추정만 하고 실제 추출 호출은 하지 않음")
    parser.add_argument("--limit", type=int, default=None, help="처리할 최대 건수 (자격조건/제출서류 절반씩)")
    parser.add_argument("--all", action="store_true", help="review_required 전체 처리")
    parser.add_argument("--confirm-full-run", action="store_true", help="--all 실행에 필요한 명시적 승인 플래그")
    args = parser.parse_args()

    dsn = os.environ.get("DATABASE_URL")
    api_key = os.environ.get("ANTHROPIC_API_KEY")
    if not dsn:
        logger.error("DATABASE_URL 환경변수가 설정되어 있지 않습니다")
        raise SystemExit(1)
    if not api_key:
        logger.error("ANTHROPIC_API_KEY 환경변수가 설정되어 있지 않습니다")
        raise SystemExit(1)
    if args.all and not args.confirm_full_run:
        logger.error("--all은 --confirm-full-run과 함께 사용해야 합니다 (전체 실행은 사용자 승인 후에만)")
        raise SystemExit(1)

    client = anthropic.Anthropic(api_key=api_key)
    conn = psycopg2.connect(dsn)
    conn.autocommit = False

    try:
        with conn.cursor() as cur:
            if args.estimate:
                estimate_mode(client, cur)
            elif args.all:
                run_mode(client, conn, cur, limit=None)
            elif args.limit:
                run_mode(client, conn, cur, limit=args.limit)
            else:
                logger.error("--estimate, --limit N, --all --confirm-full-run 중 하나를 지정하세요")
                raise SystemExit(1)
    finally:
        conn.close()


if __name__ == "__main__":
    main()
