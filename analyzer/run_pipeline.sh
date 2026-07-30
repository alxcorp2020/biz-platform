#!/usr/bin/env bash
# 텍스트 추출(run_extraction.py) + 구조화 추출(extract_sections.py)을 순서대로
# 실행하는 로컬 전용 자동화 파이프라인. 운영 배포(Render)는 distroless 이미지라
# Python이 없고, Go collector는 이미 apiserver 안에서 자체적으로 주기 실행되므로
# 이 스크립트는 로컬 개발 환경에서 cron/launchd로 등록해 쓰는 용도다.
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
  venv/bin/python extract_sections.py
  echo "=== $(date '+%Y-%m-%d %H:%M:%S') 파이프라인 종료 ==="
} >> "$LOG_FILE" 2>&1

# 오래된 로그 정리 (30일 초과분)
find "$LOG_DIR" -name 'pipeline-*.log' -mtime +30 -delete
