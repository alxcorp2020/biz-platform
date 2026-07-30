// notice_summary.go — GET /api/notices/{id} 응답에 얹는 "핵심 3줄 요약".
// analyzer/ai_summarize.py(claude-sonnet-5)가 배치로 미리 생성해 notice_versions
// 컬럼에 저장해두면, 여기서는 그 값을 그대로 읽어서 내려줄 뿐이다 — Go는
// Claude API를 직접 호출하지 않는다(비용/지연을 페이지 요청 경로에 넣지 않기 위함).
package api

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"
)

type noticeAISummary struct {
	Lines       []string   `json:"lines"`
	Model       string     `json:"model"`
	GeneratedAt *time.Time `json:"generatedAt"`
}

// fetchNoticeAISummary는 notice_detail.go의 fetchNoticeRawDetail과 같은
// 관례를 따른다 — 아직 요약이 생성되지 않았으면 (nil, nil)을 반환한다(에러 아님).
func (s *Server) fetchNoticeAISummary(ctx context.Context, versionID string) (*noticeAISummary, error) {
	var lines pq.StringArray
	var model sql.NullString
	var generatedAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT ai_summary_lines, ai_summary_model, ai_summary_generated_at
		FROM notice_versions WHERE id = $1`, versionID,
	).Scan(&lines, &model, &generatedAt)
	if err == sql.ErrNoRows || len(lines) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	summary := &noticeAISummary{Lines: []string(lines), Model: model.String}
	if generatedAt.Valid {
		summary.GeneratedAt = &generatedAt.Time
	}
	return summary, nil
}
