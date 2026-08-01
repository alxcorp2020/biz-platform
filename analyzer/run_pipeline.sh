#!/usr/bin/env bash
# 텍스트 추출(run_extraction.py)만 돌리는 로컬 전용 자동화 파이프라인.
#
# Phase 4(서류자동화 고도화)부터 구조화 추출(extract_sections.py의 규칙
# 기반 1차 + ai_extract.py의 AI 보완 2차)은 apiserver 안에 Go로 포팅돼
# 자체 1시간 주기 배치로 자동 실행된다(collector/internal/api/
# document_extraction.go, cmd/apiserver/main.go의
# startBackgroundDocumentExtraction) — 이 스크립트에서는 더 이상 안 돌린다.
# extract_sections.py/ai_extract.py를 여기서 또 수동으로 돌리면 Go 배치와
# 별도로 중복 저장되니(워터마크 컬럼을 Python 스크립트는 모름) 피할 것 —
# 특정 첨부파일 1건만 디버깅하고 싶을 때만 각 스크립트를 개별 실행.
#
# run_extraction.py(PDF/HWP → 텍스트)는 여전히 Python 전용 영역이라
# (Go에 마땅한 HWP 라이브러리가 없음, 운영 distroless 이미지엔 Python 자체가
# 없음) 로컬 cron/launchd로 계속 사람이 돌려야 한다.
#
# 등록 예시 (매시 20분에 실행, crontab -e):
#   20 * * * * /path/to/biz-platform/analyzer/run_pipeline.sh
#
# 사용법:
#   cp .env.example .env  # 최초 1회, DATABASE_URL/ATTACHMENT_DIR 값 채우기
#   ./run_pipeline.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

if [ -z "${DATABASE_URL:-}" ]; then
  echo "DATABASE_URL이 설정되어 있지 않습니다 (.env 파일을 확인하세요)" >&2
  exit 1
fi

LOG_DIR="$SCRIPT_DIR/logs"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/pipeline-$(date +%Y%m%d-%H%M%S).log"

{
  echo "=== $(date '+%Y-%m-%d %H:%M:%S') 파이프라인 시작 ==="
  venv/bin/python run_extraction.py
  echo "=== $(date '+%Y-%m-%d %H:%M:%S') 파이프라인 종료 ==="
} >> "$LOG_FILE" 2>&1

# 오래된 로그 정리 (30일 초과분)
find "$LOG_DIR" -name 'pipeline-*.log' -mtime +30 -delete
