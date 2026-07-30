#!/usr/bin/env python3
"""공고 상세 "AI 요약 브리핑" — 핵심 3줄 요약 생성 (claude-sonnet-5).

ai_extract.py(자격조건/제출서류 AI 보완 추출)와 같은 CLI/비용관리 패턴을
재사용한다. 다만 이건 "추출"이 아니라 "요약"이라 근본적인 차이가 있다:
ai_extract.py는 quoted_text가 원문에 실제로 존재하는지 문자열로 검증까지
하지만, 요약은 본질적으로 paraphrase라 같은 방식의 사후 검증이 불가능하다.
대신 모델에 넘기는 입력 자체를 이미 검증된 구조화 필드(공고 기본정보 +
eligibility_conditions/required_documents)로 좁히고, 프롬프트로 "주어진
정보 밖의 내용은 만들지 말라"고 명시해 grounding을 확보한다.

대상: notice_versions 중 is_current=true(최신 버전)이면서 아직 요약이 없는
행. 재공고로 새 버전이 생기면 그 행은 자연히 ai_summary_lines가 NULL이라
다음 실행 때 자동으로 대상이 된다 — 이미 요약된 옛 버전을 다시 돌리지
않아 비용이 절감된다.

사용법:
    venv/bin/python ai_summarize.py --estimate            # 비용 추정만 (API 호출 없음)
    venv/bin/python ai_summarize.py --limit 10             # 소규모 테스트 실행
    venv/bin/python ai_summarize.py --all --confirm-full-run   # 전체 미요약 공고 처리 (승인 필요)

환경변수:
    DATABASE_URL, ANTHROPIC_API_KEY

주의: run_pipeline.sh(자동 cron 파이프라인)에는 포함되어 있지 않다 —
ai_extract.py와 마찬가지로 비용이 드는 단계라 항상 수동으로, 견적 확인 후 실행한다.
"""
import argparse
import logging
import os
import re

import anthropic
import psycopg2

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger("ai_summarize")

MODEL = "claude-sonnet-5"

# claude-sonnet-5 가격: 2026-08-31까지 인트로 단가, 이후 정가로 복귀.
INPUT_PRICE_INTRO = 2.00
OUTPUT_PRICE_INTRO = 10.00
INPUT_PRICE_STANDARD = 3.00
OUTPUT_PRICE_STANDARD = 15.00

# 3줄 요약은 자격조건/제출서류 추출보다 출력이 짧다 — 견적 단계 가정치.
ASSUMED_OUTPUT_TOKENS_PER_ROW = 100

# NOTE: Claude strict tool-use는 array 타입의 minItems/maxItems를 0/1 외의
# 값으로는 지원하지 않는다(실제 count_tokens 호출로 확인한 API 제약 —
# "For 'array' type, 'minItems' values other than 0 or 1 are not supported").
# 그래서 배열 1개 대신 line1/line2/line3 세 개의 필수 스칼라 필드로 스키마를
# 구성한다 — required로 지정하면 정확히 3개가 보장되고, 배열 길이 제약이 필요 없다.
SUMMARY_TOOL = {
    "name": "summarize_notice",
    "description": "공고 정보를 핵심 3줄로 요약합니다. 주어진 정보에 없는 내용이나 구체적인 수치는 절대 만들어내지 마세요.",
    "strict": True,
    "input_schema": {
        "type": "object",
        "properties": {
            "line1": {"type": "string", "description": "첫 번째 줄 (80자 이내). 주어진 정보만 사용하고 새로운 사실을 추가하지 말 것."},
            "line2": {"type": "string", "description": "두 번째 줄 (80자 이내). 주어진 정보만 사용하고 새로운 사실을 추가하지 말 것."},
            "line3": {"type": "string", "description": "세 번째 줄 (80자 이내). 주어진 정보만 사용하고 새로운 사실을 추가하지 말 것."},
        },
        "required": ["line1", "line2", "line3"],
        "additionalProperties": False,
    },
}

PROMPT_TEMPLATE = (
    "다음은 대한민국 공공입찰 공고의 핵심 정보입니다.\n\n"
    "이 정보를 바탕으로 이 공고가 어떤 사업인지, 참가하려면 무엇이 필요한지 "
    "핵심 3줄로 요약하세요.\n\n"
    "중요한 규칙:\n"
    "- 아래 정보에 없는 내용이나 구체적인 수치를 절대 만들어내지 마세요.\n"
    "- 정보가 부족한 항목은 억지로 채우지 말고, 그 사실을 짧게 언급하거나 생략하세요.\n"
    "- 각 줄은 80자 이내로 간결하게 작성하세요.\n\n"
    "공고 정보:\n---\n{context}\n---"
)


def fetch_summary_targets(cur, limit=None):
    query = """
        SELECT nv.id,
               n.title, n.organization_name, n.region, n.industry, n.budget_amount, n.application_end_at,
               COALESCE(array_agg(DISTINCT ec.condition_name) FILTER (WHERE ec.condition_name IS NOT NULL), '{}') AS condition_names,
               COALESCE(array_agg(DISTINCT rd.document_name) FILTER (WHERE rd.document_name IS NOT NULL), '{}') AS document_names
        FROM notice_versions nv
        JOIN notices n ON n.id = nv.notice_id
        LEFT JOIN eligibility_conditions ec ON ec.notice_version_id = nv.id AND ec.review_status != 'rejected'
        LEFT JOIN required_documents rd ON rd.notice_version_id = nv.id AND rd.review_status != 'rejected'
        WHERE nv.is_current = true AND nv.ai_summary_lines IS NULL
        GROUP BY nv.id, n.title, n.organization_name, n.region, n.industry, n.budget_amount, n.application_end_at
        ORDER BY nv.collected_at
    """
    if limit:
        query += f" LIMIT {int(limit)}"
    cur.execute(query)
    return [
        {
            "id": r[0], "title": r[1], "organization_name": r[2], "region": r[3],
            "industry": r[4], "budget_amount": r[5], "application_end_at": r[6],
            "condition_names": r[7], "document_names": r[8],
        }
        for r in cur.fetchall()
    ]


def format_budget(amount):
    return f"{amount:,}원" if amount is not None else "확인 안 됨"


def format_date(dt):
    return dt.strftime("%Y-%m-%d") if dt is not None else "확인 안 됨"


def build_context(row):
    lines = [
        f"공고명: {row['title']}",
        f"발주기관: {row['organization_name'] or '확인 안 됨'}",
        f"지역: {row['region'] or '확인 안 됨'}",
        f"업종: {row['industry'] or '확인 안 됨'}",
        f"예산: {format_budget(row['budget_amount'])}",
        f"마감일: {format_date(row['application_end_at'])}",
    ]
    if row["condition_names"]:
        lines.append("참가자격 조건: " + "; ".join(row["condition_names"]))
    if row["document_names"]:
        lines.append("제출서류: " + "; ".join(row["document_names"]))
    return "\n".join(lines)


def count_tokens(client, context):
    result = client.messages.count_tokens(
        model=MODEL, tools=[SUMMARY_TOOL],
        messages=[{"role": "user", "content": PROMPT_TEMPLATE.format(context=context)}],
    )
    return result.input_tokens


def call_claude(client, context):
    response = client.messages.create(
        model=MODEL,
        max_tokens=1024,
        thinking={"type": "disabled"},
        output_config={"effort": "low"},
        tools=[SUMMARY_TOOL],
        tool_choice={"type": "tool", "name": SUMMARY_TOOL["name"]},
        messages=[{"role": "user", "content": PROMPT_TEMPLATE.format(context=context)}],
    )
    lines = []
    for block in response.content:
        if block.type == "tool_use":
            lines = [block.input.get("line1"), block.input.get("line2"), block.input.get("line3")]
    return lines, response.usage


# 실제 운영 실행 중 발견된 사례: 모델이 '<...', '</antml>', '</anT>' 같은
# 깨진 태그 조각을 반환했는데, "비어있지 않은 문자열 3개"라는 조건은
# 통과해서 그대로 저장된 적이 있다(정상적인 요약 문장이 아닌데도 형식만
# 맞아 걸러지지 않음). 이런 걸 저장하지 않도록 명백히 비정상적인 패턴을
# 최소한으로 걸러낸다 — 완벽한 품질 검증은 아니고, 이 사례처럼 태그 조각이
# 섞여 나오는 명백한 오작동만 잡아내는 안전장치다.
_SUSPICIOUS_PATTERN_RE = re.compile(r"[<>]|antml", re.IGNORECASE)


def validate_lines(lines):
    """3줄 모두 비어있지 않은 문자열이고, 태그 조각 등 명백히 비정상적인
    패턴이 섞이지 않았는지 확인. 모델이 형식을 어기거나 이상한 값을 반환하면
    지어내서 채우지 않고 그 항목 자체를 버린다."""
    if not isinstance(lines, list) or len(lines) != 3:
        return None
    cleaned = [s.strip() for s in lines if isinstance(s, str) and s.strip()]
    if len(cleaned) != 3:
        return None
    if any(_SUSPICIOUS_PATTERN_RE.search(s) for s in cleaned):
        return None
    return cleaned


def save_summary(cur, version_id, lines):
    cur.execute(
        """UPDATE notice_versions
               SET ai_summary_lines = %s, ai_summary_model = %s, ai_summary_generated_at = now()
             WHERE id = %s""",
        (lines, MODEL, version_id),
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
    rows = fetch_summary_targets(cur)
    logger.info("요약 대상(최신 버전 중 미요약): %d건", len(rows))

    total_input = 0
    for i, row in enumerate(rows, 1):
        context = build_context(row)
        total_input += count_tokens(client, context)
        if i % 50 == 0:
            logger.info("  토큰 계산 진행: %d/%d", i, len(rows))

    assumed_output = len(rows) * ASSUMED_OUTPUT_TOKENS_PER_ROW
    print_cost_report(
        "전체 미요약 공고 (출력 토큰은 건당 %d로 가정, 실측 아님)" % ASSUMED_OUTPUT_TOKENS_PER_ROW,
        len(rows), total_input, assumed_output,
    )


def run_mode(client, conn, cur, limit):
    rows = fetch_summary_targets(cur, limit=limit)
    logger.info("처리 대상: %d건", len(rows))

    total_saved, total_discarded = 0, 0
    total_input_tokens, total_output_tokens = 0, 0

    for row in rows:
        context = build_context(row)
        lines, usage = call_claude(client, context)
        total_input_tokens += usage.input_tokens
        total_output_tokens += usage.output_tokens

        validated = validate_lines(lines)
        if validated is None:
            total_discarded += 1
            logger.info("[%s] 형식 오류 또는 비정상 패턴(태그 조각 등)이 섞여 버림: %r", row["id"], lines)
            continue

        save_summary(cur, row["id"], validated)
        conn.commit()
        total_saved += 1
        logger.info("[%s] 저장: %s", row["id"], " / ".join(validated))

    logger.info("종료: 처리 %d건, 저장 %d건, 버림(형식 오류/비정상 패턴) %d건", len(rows), total_saved, total_discarded)
    logger.info("실측 토큰: 입력 %s, 출력 %s", f"{total_input_tokens:,}", f"{total_output_tokens:,}")


def main():
    parser = argparse.ArgumentParser(description="공고 핵심 3줄 요약 생성 (claude-sonnet-5)")
    parser.add_argument("--estimate", action="store_true", help="비용 추정만 하고 실제 호출은 하지 않음")
    parser.add_argument("--limit", type=int, default=None, help="처리할 최대 건수")
    parser.add_argument("--all", action="store_true", help="미요약 공고 전체 처리")
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
