// proposal_evaluation.go — 평가기준 맞춤 제안서 1차(2026-08-16): 공고별 "제안서
// 평가기준" 구조화.
//
// 기존 구조 조사 결과 평가기준(평가항목/배점/세부기준)을 구조화해 둔 데이터는
// 없었다(eligibility_conditions/required_documents는 참가자격·제출서류만, ai_summary는
// 3줄 요약). 그래서 여기서 처음 추출하되, 새 테이블 대신 notice_versions의
// evaluation_criteria(JSONB)/…_status/…_extracted_at 3컬럼(ai_summary_* 관례)에
// 공고 버전 단위로 저장한다 — 정정공고로 버전이 바뀌면 새 버전 행에서 다시 추출.
//
// 추출 원칙(사실성):
//   - 원문(첨부 extracted_text 우선, 제안요청서/평가표 파일 우선순위)에서만 뽑는다.
//   - 배점을 못 찾으면 0으로 추측하지 않고 null(미확인)로 둔다.
//   - AI 추출(기존 anthropic.Client tool-use, 다른 추출기와 같은 패턴)은 각 항목의
//     인용문(sourceText) 또는 제목이 원문에 실제로 존재할 때만 채택한다
//     (verifyAndLocateQuote 재사용). 그 외는 버린다.
//   - AI를 못 쓰면(키 없음/실패) 보수적 규칙 추출로 폴백한다("항목명(N점)" 형태와
//     anchor 창 안의 "항목명 N" 형태만).
//   - 평가기준이 없는 공고는 정상 상태(status=not_found)다. 일반 템플릿을 만들지 않는다.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// evaluationCriterion — 평가항목 하나.
type evaluationCriterion struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Score       *float64 `json:"score"` // nil = 배점 미확인(추측 금지)
	Category    string   `json:"category"`
	SubCriteria []string `json:"subCriteria"`
	SourceDoc   string   `json:"sourceDocument,omitempty"`
	SourceText  string   `json:"sourceText,omitempty"`
	Confidence  string   `json:"confidence"` // high | medium | low
}

// proposalRequirement — 공고별 제안서 작성 요구사항(확실한 것만: 페이지 제한·글자
// 크기·제출 부수·파일형식·목차·블라인드 등). 못 찾으면 비어 있다.
type proposalRequirement struct {
	Label      string `json:"label"`
	Value      string `json:"value"`
	SourceText string `json:"sourceText,omitempty"`
}

// evaluationCriteriaSet — 공고 버전 단위 추출 결과(그대로 JSONB 저장).
type evaluationCriteriaSet struct {
	Criteria     []evaluationCriterion `json:"criteria"`
	TotalScore   *float64              `json:"totalScore"` // 모든 항목 배점이 있을 때만 합계
	Notes        []string              `json:"notes,omitempty"`
	Requirements []proposalRequirement `json:"requirements"`
	Method       string                `json:"method"` // ai | rule
	Model        string                `json:"model,omitempty"`
	SourceDocs   []string              `json:"sourceDocuments"`
	ExtractedAt  time.Time             `json:"extractedAt"`
}

const (
	evalStatusFound    = "found"
	evalStatusNotFound = "not_found"
	evalStatusError    = "error"
	// evalStatusPending — 이 버전의 첨부 텍스트 추출(Cron 워커)이 아직 진행 중이라 평가기준을
	// 판단할 수 없는 상태. **응답 전용**이며 notice_versions에 저장하지 않는다(부정 캐시 금지) —
	// 텍스트가 도착한 뒤의 첫 요청이 정상 추출을 수행한다. 이 상태에서는 모델 호출도 하지 않는다.
	evalStatusPending = "pending"

	// evalNotFoundRecheckAfter — not_found/error는 첨부 텍스트 추출(1시간 배치)이 뒤늦게
	// 도착할 수 있어 이 시간이 지나면 다시 시도한다.
	evalNotFoundRecheckAfter = 6 * time.Hour
	// evalPendingExtractionMaxAge — 첨부가 pending/processing인 채로 이보다 오래됐으면
	// "추출 진행 중"으로 보지 않는다(기존 not_found 정책으로 처리). Cron 워커는 새 첨부를 보통
	// 15분 안에 처리하므로, 이보다 오래 pending인 첨부는 워커 컷오버 이전 backlog이거나 정체된
	// 것 — 그런 공고에서 readiness가 영원히 "분석 중"으로 남지 않게 하는 상한.
	evalPendingExtractionMaxAge = 48 * time.Hour
	evalContextMaxRunes      = 14000
	evalWindowLines          = 70
	evalAITimeout            = 60 * time.Second
)

var (
	evalAnchorRe = regexp.MustCompile(`평가\s*항목|배\s*점|평가\s*기준|기술\s*평가|정성\s*평가|정량\s*평가|제안서\s*평가|평가\s*내용`)
	// "항목명(N점)" / "항목명 (N 점)" — 가장 확실한 형태.
	evalParenScoreRe = regexp.MustCompile(`^\s*(?:[\(（]?\d{1,2}[\)）.]\s*|[가-하][.)]\s*|[①-⑳]\s*|[-•·○ㅇ]\s*)?([가-힣A-Za-z0-9·/&\s]{2,40}?)\s*[\(（]\s*(\d{1,3}(?:\.\d)?)\s*점\s*[\)）]`)
	// "항목명   N" (표에서 배점 열이 같은 줄에 붙은 경우) — anchor 창 안에서만, 카테고리
	// 키워드가 있을 때만 채택(페이지 번호·조항 번호 오탐 방지).
	evalBareScoreRe    = regexp.MustCompile(`^\s*(?:[\(（]?\d{1,2}[\)）.]\s*|[가-하][.)]\s*|[①-⑳]\s*|[-•·○ㅇ]\s*)?([가-힣A-Za-z·/&\s]{2,40}?)\s+(\d{1,3}(?:\.\d)?)\s*(?:점)?\s*$`)
	evalTitleKeywordRe = regexp.MustCompile(`이해|계획|수행|실적|인력|조직|관리|평가|가격|능력|전략|방안|체계|품질|안전|보안|사후|유지|경영|신용|기술|제안|운영|일정|추진|구성|전문|타당|적정|창의|독창|효과|활용|지원|교육|홍보|기획|디자인|설계|검증`)
	evalTotalWordRe    = regexp.MustCompile(`^(소?계|합계|총점|총\s*계|총\s*배점|배점|평가항목|구분|점수)$`)
	// 작성 요구사항(확실한 형태만).
	evalReqPageRe   = regexp.MustCompile(`(\d{1,3})\s*(?:페이지|쪽|매)\s*(?:이내|이하|내외)`)
	evalReqFontRe   = regexp.MustCompile(`(?:글자\s*크기|글꼴\s*크기|폰트)\s*[:：]?\s*(\d{1,2})\s*(?:pt|포인트|point)`)
	evalReqCopiesRe = regexp.MustCompile(`(?:제안서|제출\s*부수|부수)\s*[:：]?\s*(?:원본\s*\d+\s*부[,·\s]*)?(?:사본\s*)?(\d{1,2})\s*부`)
	evalReqFileRe   = regexp.MustCompile(`(?i)(hwpx?|pdf|docx?|pptx?)\s*(?:파일|형식|포맷)`)
	evalReqBlindRe  = regexp.MustCompile(`블라인드|회사명(?:을|은|이)?\s*(?:표기|기재|노출)\s*(?:금지|불가|하지)`)
)

// fetchStoredEvaluationCriteria — 저장된 결과. (nil, "", zero, nil)이면 아직 없음.
func (s *Server) fetchStoredEvaluationCriteria(ctx context.Context, versionID string) (*evaluationCriteriaSet, string, time.Time, error) {
	var raw []byte
	var status sql.NullString
	var extractedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT evaluation_criteria, evaluation_criteria_status, evaluation_criteria_extracted_at FROM notice_versions WHERE id = $1`, versionID).Scan(&raw, &status, &extractedAt)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	var set *evaluationCriteriaSet
	if len(raw) > 0 {
		set = &evaluationCriteriaSet{}
		if err := json.Unmarshal(raw, set); err != nil {
			set = nil
		}
	}
	var at time.Time
	if extractedAt.Valid {
		at = extractedAt.Time
	}
	return set, status.String, at, nil
}

func (s *Server) storeEvaluationCriteria(ctx context.Context, versionID string, set *evaluationCriteriaSet, status string) error {
	var raw any
	if set != nil {
		b, err := json.Marshal(set)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE notice_versions SET evaluation_criteria = $2::jsonb, evaluation_criteria_status = $3, evaluation_criteria_extracted_at = now() WHERE id = $1`, versionID, raw, status)
	return err
}

// evalSourceDoc — 추출 대상 텍스트 한 덩어리(첨부 1개 또는 공고 원문).
type evalSourceDoc struct {
	name string
	text string
	rank int // 낮을수록 우선(제안요청서/평가표 파일)
}

// evaluationSourceExtractionInProgress — 이 버전의 첨부 중 텍스트 추출이 아직 안 끝난 것
// (extraction_status pending/processing)이 있는지. completed/failed/unsupported는 종결 상태라
// 제외. evalPendingExtractionMaxAge보다 오래된 pending은 워커 컷오버 이전 backlog/정체로 보고
// "진행 중"에서 제외한다(그 공고는 기존 not_found 정책으로).
func (s *Server) evaluationSourceExtractionInProgress(ctx context.Context, versionID string) (bool, error) {
	var inProgress bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM attachments
			WHERE notice_version_id = $1
			  AND extraction_status IN ('pending','processing')
			  AND created_at > $2)`,
		versionID, time.Now().Add(-evalPendingExtractionMaxAge)).Scan(&inProgress)
	if err != nil {
		return false, err
	}
	return inProgress, nil
}

// fetchEvaluationSourceDocs — 현재 버전의 첨부 extracted_text(있는 것만). 제안요청서/
// 평가/RFP/과업 파일이 앞에 오도록 정렬. 공고 raw_content는 나라장터 API JSON이라
// 평가기준이 들어있는 경우가 거의 없어 anchor가 있을 때만 마지막에 붙인다.
func (s *Server) fetchEvaluationSourceDocs(ctx context.Context, versionID string) ([]evalSourceDoc, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT original_filename, extracted_text FROM attachments WHERE notice_version_id = $1 AND extracted_text IS NOT NULL AND length(extracted_text) > 0 ORDER BY created_at`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []evalSourceDoc
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			return nil, err
		}
		docs = append(docs, evalSourceDoc{name: name, text: text, rank: evalDocRank(name)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT rd.raw_content FROM notice_versions nv JOIN raw_documents rd ON rd.id = nv.raw_document_id WHERE nv.id = $1`, versionID).Scan(&raw); err == nil && raw.Valid && evalAnchorRe.MatchString(raw.String) {
		docs = append(docs, evalSourceDoc{name: "공고문", text: raw.String, rank: 9})
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].rank < docs[j].rank })
	return docs, nil
}

func evalDocRank(name string) int {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "평가"):
		return 0
	case strings.Contains(n, "제안요청") || strings.Contains(n, "rfp"):
		return 1
	case strings.Contains(n, "과업") || strings.Contains(n, "지시서") || strings.Contains(n, "사양"):
		return 2
	case strings.Contains(n, "공고"):
		return 3
	}
	return 5
}

// buildEvaluationContext — anchor 주변 창(±evalWindowLines줄)만 모아 컨텍스트를 만든다
// (전체 문서를 넣지 않음: 비용/정확도). 반환: 컨텍스트, 사용한 문서명 목록, anchor 유무.
func buildEvaluationContext(docs []evalSourceDoc) (string, []string, bool) {
	var b strings.Builder
	var used []string
	found := false
	for _, d := range docs {
		lines := strings.Split(d.text, "\n")
		var take []bool
		for i, ln := range lines {
			if evalAnchorRe.MatchString(ln) {
				found = true
				if take == nil {
					take = make([]bool, len(lines))
				}
				lo, hi := i-8, i+evalWindowLines
				if lo < 0 {
					lo = 0
				}
				if hi > len(lines) {
					hi = len(lines)
				}
				for k := lo; k < hi; k++ {
					take[k] = true
				}
			}
		}
		if take == nil {
			continue
		}
		used = append(used, d.name)
		fmt.Fprintf(&b, "\n=== 문서: %s ===\n", d.name)
		prev := false
		for i, ln := range lines {
			if take[i] {
				b.WriteString(ln)
				b.WriteString("\n")
				prev = true
			} else if prev {
				b.WriteString("…\n")
				prev = false
			}
		}
		if len([]rune(b.String())) > evalContextMaxRunes {
			break
		}
	}
	return truncateRunes(b.String(), evalContextMaxRunes), used, found
}

// evalInflight — 같은 공고 버전에 대한 동시 추출 요청(같은/다른 유료 사용자)이 외부 모델을
// 중복 호출하지 않도록 진행 중 표시(공유 뮤텍스). notice_opening_result.go의 in-flight 패턴과
// 동일. 프로세스 단위 보호이며 멀티 인스턴스 간 중복은 막지 않는다(그 경우도 결과는 같은
// 버전 행에 저장돼 마지막 쓰기가 남을 뿐 데이터가 섞이진 않는다).
var (
	evalInflightMu sync.Mutex
	evalInflight   = map[string]chan struct{}{}
	// evalExtractionObserver — 테스트 훅: 실제 추출 작업(원문 수집→모델/규칙)이 시작될 때마다
	// 호출된다. 운영에서는 nil.
	evalExtractionObserver func(versionID string)
)

func acquireEvalInflight(versionID string) (chan struct{}, bool) {
	evalInflightMu.Lock()
	defer evalInflightMu.Unlock()
	if ch, ok := evalInflight[versionID]; ok {
		return ch, false
	}
	ch := make(chan struct{})
	evalInflight[versionID] = ch
	return ch, true
}

func releaseEvalInflight(versionID string, ch chan struct{}) {
	evalInflightMu.Lock()
	delete(evalInflight, versionID)
	evalInflightMu.Unlock()
	close(ch)
}

// evalCachedDecision — 저장값으로 응답 가능한지. found면 항상, not_found/error는 재확인 TTL 안이면.
func evalCachedDecision(set *evaluationCriteriaSet, status string, at time.Time, force bool) bool {
	if force {
		return false
	}
	if status == evalStatusFound && set != nil {
		return true
	}
	return (status == evalStatusNotFound || status == evalStatusError) && time.Since(at) < evalNotFoundRecheckAfter
}

// getOrExtractEvaluationCriteria — 저장값 우선, 없거나 재확인 시점이면 추출해 저장.
// 반환 status: found / not_found / error(추출 실패). error 상태여도 err=nil로 돌려주고
// 호출부가 "확인 불가"로 안내한다(상세/제안서 흐름을 500으로 깨지 않게).
// 동시성: 같은 버전의 동시 요청은 첫 요청만 추출하고 나머지는 끝날 때까지 기다렸다가 저장값을
// 읽는다(외부 모델 호출 최대 1회). 이미 found면 모델 호출 0.
func (s *Server) getOrExtractEvaluationCriteria(ctx context.Context, versionID string, force bool) (*evaluationCriteriaSet, string, error) {
	set, status, at, err := s.fetchStoredEvaluationCriteria(ctx, versionID)
	if err != nil {
		return nil, "", err
	}
	if evalCachedDecision(set, status, at, force) {
		return set, status, nil
	}
	ch, mine := acquireEvalInflight(versionID)
	if !mine {
		select {
		case <-ch:
		case <-ctx.Done():
			return set, status, ctx.Err()
		}
		// 선행 요청이 저장한 결과를 그대로 재사용(force여도 방금 끝난 추출 결과면 충분).
		set2, status2, _, err := s.fetchStoredEvaluationCriteria(ctx, versionID)
		if err != nil {
			return nil, "", err
		}
		if status2 == "" {
			// 선행 요청이 아무것도 저장하지 않고 끝남 = 첨부 텍스트 추출 진행 중(pending, 미저장) —
			// 대기자도 같은 판단을 돌려준다(빈 상태를 "평가기준 없음"으로 오해하지 않게).
			if inProgress, err := s.evaluationSourceExtractionInProgress(ctx, versionID); err == nil && inProgress {
				return nil, evalStatusPending, nil
			}
		}
		return set2, status2, nil
	}
	defer releaseEvalInflight(versionID, ch)
	// 대기 중이던 다른 요청이 방금 저장했을 수 있으니 한 번 더 확인(경합 창 축소).
	if set3, status3, at3, err := s.fetchStoredEvaluationCriteria(ctx, versionID); err == nil && evalCachedDecision(set3, status3, at3, force) {
		return set3, status3, nil
	}
	// prevFound — 강제 재추출이 실패해도 기존 완료 결과는 덮어쓰지 않는다.
	var prevFound *evaluationCriteriaSet
	if status == evalStatusFound && set != nil {
		prevFound = set
	}
	// 첨부 텍스트 추출이 아직 진행 중이면 판단을 보류한다(모델 호출 0·저장 0). 이 시점에 추출하면
	// 제안요청서가 도착하기 전의 공고서만으로 not_found(6h 부정 캐시)나 부분 결과 found(영구 캐시)를
	// 만들 수 있다. 이미 found가 있으면(강제 재확인) 기존 결과를 그대로 돌려주고, 새 첨부 텍스트가
	// 다 도착한 뒤의 재확인에서 재추출한다.
	if inProgress, err := s.evaluationSourceExtractionInProgress(ctx, versionID); err != nil {
		return nil, "", err
	} else if inProgress {
		if prevFound != nil {
			return prevFound, evalStatusFound, nil
		}
		return nil, evalStatusPending, nil
	}
	if evalExtractionObserver != nil {
		evalExtractionObserver(versionID)
	}
	docs, err := s.fetchEvaluationSourceDocs(ctx, versionID)
	if err != nil {
		return nil, "", err
	}
	contextText, used, hasAnchor := buildEvaluationContext(docs)
	if !hasAnchor || strings.TrimSpace(contextText) == "" {
		if prevFound != nil {
			return prevFound, evalStatusFound, nil
		}
		_ = s.storeEvaluationCriteria(ctx, versionID, nil, evalStatusNotFound)
		return nil, evalStatusNotFound, nil
	}
	var result *evaluationCriteriaSet
	var extractErr error
	if s.anthropicClient != nil && os.Getenv("ANTHROPIC_API_KEY") != "" {
		actx, cancel := context.WithTimeout(ctx, evalAITimeout)
		result, extractErr = s.extractEvaluationCriteriaAI(actx, contextText)
		cancel()
		if extractErr != nil {
			s.logger.Warn("evaluation criteria: AI extraction failed; falling back to rules", "versionId", versionID, "error", extractErr)
		}
	}
	if result == nil || len(result.Criteria) == 0 {
		rule := extractEvaluationCriteriaRule(contextText)
		if len(rule.Criteria) > 0 {
			result = rule
		}
	}
	if result == nil || len(result.Criteria) == 0 {
		if prevFound != nil {
			// 재추출 실패/무결과 — 기존 완료 결과 유지(덮어쓰기 금지).
			s.logger.Warn("evaluation criteria: re-extraction produced nothing; keeping previous result", "versionId", versionID)
			return prevFound, evalStatusFound, nil
		}
		if extractErr != nil {
			_ = s.storeEvaluationCriteria(ctx, versionID, nil, evalStatusError)
			return nil, evalStatusError, nil
		}
		_ = s.storeEvaluationCriteria(ctx, versionID, nil, evalStatusNotFound)
		return nil, evalStatusNotFound, nil
	}
	if len(result.Requirements) == 0 {
		result.Requirements = extractProposalRequirementsRule(contextText)
	}
	if result.Requirements == nil {
		result.Requirements = []proposalRequirement{}
	}
	result.SourceDocs = used
	result.ExtractedAt = time.Now()
	finalizeCriteriaSet(result)
	if err := s.storeEvaluationCriteria(ctx, versionID, result, evalStatusFound); err != nil {
		s.logger.Error("evaluation criteria: store failed", "error", err)
	}
	return result, evalStatusFound, nil
}

// finalizeCriteriaSet — id 부여, 총점(모든 항목 배점이 있을 때만), 빈 배열 정규화.
func finalizeCriteriaSet(set *evaluationCriteriaSet) {
	total := 0.0
	allKnown := len(set.Criteria) > 0
	for i := range set.Criteria {
		c := &set.Criteria[i]
		c.ID = "c" + strconv.Itoa(i+1)
		if c.SubCriteria == nil {
			c.SubCriteria = []string{}
		}
		if c.Category == "" {
			c.Category = classifyCriterionCategory(c.Title)
		}
		if c.Score != nil {
			total += *c.Score
		} else {
			allKnown = false
		}
	}
	if allKnown {
		t := total
		set.TotalScore = &t
	} else {
		set.TotalScore = nil
	}
}

func classifyCriterionCategory(title string) string {
	t := strings.ReplaceAll(title, " ", "")
	switch {
	case strings.Contains(t, "가격") || strings.Contains(t, "입찰금액"):
		return "price"
	case strings.Contains(t, "정량") || strings.Contains(t, "경영상태") || strings.Contains(t, "신용") || strings.Contains(t, "재무"):
		return "quantitative"
	}
	return "qualitative"
}

// ---- 규칙 추출(폴백) ----

func extractEvaluationCriteriaRule(contextText string) *evaluationCriteriaSet {
	set := &evaluationCriteriaSet{Method: "rule", Requirements: []proposalRequirement{}}
	seen := map[string]bool{}
	add := func(title string, score float64, conf, src string) {
		title = strings.TrimSpace(strings.Trim(title, "-•·○ㅇ:："))
		key := strings.ReplaceAll(title, " ", "")
		if len([]rune(key)) < 2 || evalTotalWordRe.MatchString(key) || seen[key] {
			return
		}
		seen[key] = true
		sc := score
		set.Criteria = append(set.Criteria, evaluationCriterion{Title: title, Score: &sc, Confidence: conf, SourceText: strings.TrimSpace(src), SubCriteria: []string{}})
	}
	for _, ln := range strings.Split(contextText, "\n") {
		if m := evalParenScoreRe.FindStringSubmatch(ln); m != nil {
			if v, err := strconv.ParseFloat(m[2], 64); err == nil && v > 0 && v <= 100 {
				add(m[1], v, "medium", ln)
			}
			continue
		}
		if m := evalBareScoreRe.FindStringSubmatch(ln); m != nil && evalTitleKeywordRe.MatchString(m[1]) {
			if v, err := strconv.ParseFloat(m[2], 64); err == nil && v > 0 && v <= 100 {
				add(m[1], v, "low", ln)
			}
		}
	}
	// 100점 초과 합계(중복 표기: 총괄표+세부표)면 상위 항목만 남기는 시도는 하지 않고,
	// 그대로 두되 총점 계산은 finalizeCriteriaSet이 한다(사용자 확인용).
	return set
}

func extractProposalRequirementsRule(contextText string) []proposalRequirement {
	var out []proposalRequirement
	seen := map[string]bool{}
	push := func(label, value, src string) {
		k := label + "|" + value
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, proposalRequirement{Label: label, Value: value, SourceText: strings.TrimSpace(src)})
	}
	for _, ln := range strings.Split(contextText, "\n") {
		if m := evalReqPageRe.FindStringSubmatch(ln); m != nil && strings.Contains(ln, "제안") {
			push("분량", m[1]+"페이지 이내", ln)
		}
		if m := evalReqFontRe.FindStringSubmatch(ln); m != nil {
			push("글자 크기", m[1]+"pt", ln)
		}
		if m := evalReqCopiesRe.FindStringSubmatch(ln); m != nil && strings.Contains(ln, "제안") {
			push("제출 부수", m[1]+"부", ln)
		}
		if m := evalReqFileRe.FindStringSubmatch(ln); m != nil && strings.Contains(ln, "제안") {
			push("파일 형식", strings.ToUpper(m[1]), ln)
		}
		if evalReqBlindRe.MatchString(ln) {
			push("블라인드/회사명 표기 제한", "공고문 확인", ln)
		}
	}
	if out == nil {
		out = []proposalRequirement{}
	}
	return out
}

// ---- AI 추출(tool-use, 기존 anthropic 클라이언트 재사용) ----

const evaluationPromptTemplate = "다음은 공공 입찰 공고의 제안요청서/평가표 원문 일부입니다(여러 문서를 '=== 문서: 이름 ===' 로 구분, PDF 표는 셀이 줄바꿈으로 흩어져 있을 수 있음).\n\n" +
	"이 원문에서 '제안서 평가항목'과 '배점'을 구조화하세요.\n\n" +
	"중요한 규칙:\n" +
	"- 원문에 없는 항목/배점/세부기준을 절대 만들지 마세요.\n" +
	"- 배점을 원문에서 확인할 수 없으면 score를 null로 두세요(0이나 추정값 금지).\n" +
	"- 총괄표와 세부표가 둘 다 있으면 총괄표(상위 항목) 기준으로 항목을 잡고, 세부항목은 subCriteria에 넣으세요. 같은 항목을 두 번 넣지 마세요.\n" +
	"- '계/합계/총점' 행은 항목으로 넣지 말고 totalScore에만 반영하세요.\n" +
	"- sourceText는 그 항목의 제목이 실제로 등장하는 원문 구절을 그대로 복사하세요(요약·수정 금지, 200자 이내).\n" +
	"- writingRequirements(분량/글자 크기/제출 부수/파일형식/목차/블라인드 등)는 원문에 명시된 것만, sourceText와 함께.\n" +
	"- 평가기준 표가 전혀 없으면 criteria를 빈 배열로 반환하세요.\n\n" +
	"원문:\n---\n%s\n---"

func evaluationCriteriaTool() anthropic.ToolParam {
	return anthropic.ToolParam{
		Name:        "extract_evaluation_criteria",
		Description: anthropic.String("원문에서 실제로 확인되는 제안서 평가항목·배점·세부기준만 구조화합니다. 원문에 없는 내용은 절대 만들지 마세요."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"criteria": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":       map[string]any{"type": "string", "description": "평가항목 제목(원문 표기, 60자 이내)"},
							"score":       map[string]any{"type": []string{"number", "null"}, "description": "배점. 원문에서 확인 불가면 null"},
							"category":    map[string]any{"type": "string", "enum": []string{"qualitative", "quantitative", "price", "unknown"}},
							"description": map[string]any{"type": "string", "description": "평가내용 요약(원문 근거, 200자 이내). 없으면 빈 문자열"},
							"subCriteria": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "세부 평가기준(원문 표기)"},
							"sourceText":  map[string]any{"type": "string", "description": "원문에서 그대로 복사한 구절(항목 제목 포함)"},
						},
						"required":             []string{"title", "score", "category", "description", "subCriteria", "sourceText"},
						"additionalProperties": false,
					},
				},
				"totalScore": map[string]any{"type": []string{"number", "null"}, "description": "원문에 명시된 총점(없으면 null)"},
				"notes":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "평가 방식 메모(예: 기술 90%+가격 10%). 원문 근거만"},
				"writingRequirements": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"label":      map[string]any{"type": "string"},
							"value":      map[string]any{"type": "string"},
							"sourceText": map[string]any{"type": "string"},
						},
						"required":             []string{"label", "value", "sourceText"},
						"additionalProperties": false,
					},
				},
			},
			Required:    []string{"criteria", "totalScore", "notes", "writingRequirements"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
		Strict: anthropic.Bool(true),
	}
}

func (s *Server) extractEvaluationCriteriaAI(ctx context.Context, contextText string) (*evaluationCriteriaSet, error) {
	tool := evaluationCriteriaTool()
	disabledThinking := anthropic.NewThinkingConfigDisabledParam()
	resp, err := s.anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:        companyDocumentModel,
		MaxTokens:    4096,
		Thinking:     anthropic.ThinkingConfigParamUnion{OfDisabled: &disabledThinking},
		OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffortLow},
		Tools:        []anthropic.ToolUnionParam{{OfTool: &tool}},
		ToolChoice:   anthropic.ToolChoiceParamOfTool("extract_evaluation_criteria"),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(fmt.Sprintf(evaluationPromptTemplate, contextText))),
		},
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return nil, fmt.Errorf("claude api error (status %d): %w", apiErr.StatusCode, err)
		}
		return nil, err
	}
	for _, block := range resp.Content {
		tu, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		var out struct {
			Criteria []struct {
				Title       string   `json:"title"`
				Score       *float64 `json:"score"`
				Category    string   `json:"category"`
				Description string   `json:"description"`
				SubCriteria []string `json:"subCriteria"`
				SourceText  string   `json:"sourceText"`
			} `json:"criteria"`
			TotalScore          *float64              `json:"totalScore"`
			Notes               []string              `json:"notes"`
			WritingRequirements []proposalRequirement `json:"writingRequirements"`
		}
		if err := json.Unmarshal(tu.Input, &out); err != nil {
			return nil, fmt.Errorf("parse tool input: %w", err)
		}
		set := &evaluationCriteriaSet{Method: "ai", Model: companyDocumentModel, Notes: out.Notes, Requirements: []proposalRequirement{}}
		normCtx := normalizeWhitespace(contextText)
		for _, c := range out.Criteria {
			title := strings.TrimSpace(c.Title)
			if title == "" {
				continue
			}
			// grounding: 인용문 또는 제목이 원문에 실제 존재해야 채택.
			src, ok := verifyAndLocateQuote(c.SourceText, contextText)
			titleFound := strings.Contains(strings.ReplaceAll(normCtx, " ", ""), strings.ReplaceAll(normalizeWhitespace(title), " ", ""))
			if !ok && !titleFound {
				s.logger.Info("evaluation criteria: dropped ungrounded item", "title", title)
				continue
			}
			conf := "high"
			if !ok {
				conf = "medium"
				src = ""
			}
			var score *float64
			if c.Score != nil && *c.Score > 0 && *c.Score <= 100 {
				v := *c.Score
				score = &v
			}
			cat := c.Category
			if cat == "" || cat == "unknown" {
				cat = classifyCriterionCategory(title)
			}
			set.Criteria = append(set.Criteria, evaluationCriterion{
				Title: title, Description: strings.TrimSpace(c.Description), Score: score, Category: cat,
				SubCriteria: c.SubCriteria, SourceText: truncateRunes(src, 200), Confidence: conf,
			})
		}
		for _, r := range out.WritingRequirements {
			if strings.TrimSpace(r.Label) == "" || strings.TrimSpace(r.Value) == "" {
				continue
			}
			if _, ok := verifyAndLocateQuote(r.SourceText, contextText); !ok {
				continue // 근거 없는 요구사항은 버린다(추측 금지)
			}
			set.Requirements = append(set.Requirements, proposalRequirement{Label: r.Label, Value: r.Value, SourceText: truncateRunes(r.SourceText, 200)})
		}
		return set, nil
	}
	return nil, fmt.Errorf("no tool_use block in response (stop_reason=%s)", resp.StopReason)
}
