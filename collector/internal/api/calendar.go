// calendar.go — GET /api/pipeline/{id}/calendar.ics. OAuth 연동(Google/
// Outlook) 없이, 표준 iCalendar(RFC 5545) 파일을 생성해 다운로드시키는
// 최소 버전 캘린더 연동. 사용자가 이 파일을 받아 아무 캘린더 앱에나
// "가져오기"하면 된다.
//
// 일정 3종:
//   - 제출마감: notice_pipeline_entries.submission_deadline(파이프라인
//     생성 시점에 공고 마감일을 복사해둔 값, DATE) — 종일 일정.
//   - 등록마감/개찰일시: notices 테이블에 컬럼으로 없고, g2b 원본 JSON을
//     그때그때 파싱하는 notice_detail.go의 fetchNoticeRawDetail이 유일한
//     출처다. g2b 소스가 아니거나(데모 데이터) 원문에 값이 없으면 nil이
//     되는데, 이 경우 해당 일정을 그냥 생략한다 — 날짜를 추정/기본값으로
//     채우지 않는다(이 프로젝트 전반의 원칙: 없는 데이터를 있는 것처럼
//     꾸미지 않는다).
package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

type icsEvent struct {
	uid     string
	summary string
	allDay  bool
	start   time.Time
	// pipelineEntryID/noticeTitle/kind — renderICS(.ics 출력)는 안 쓰고,
	// GET /api/pipeline/calendar-events(JSON, 인앱 캘린더 화면 전용)만
	// 쓴다. buildPipelineCalendarEvents 한 곳에서 채워두면 ICS/JSON 두
	// 출력이 항상 같은 데이터 소스·같은 판정 로직을 공유한다.
	pipelineEntryID string
	noticeTitle     string
	kind            string // "submission" | "qualification" | "opening"
}

func (s *Server) handleGetPipelineCalendar(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	entryID := r.PathValue("id")
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("pipeline-calendar: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "company_profile_required"})
		return
	}

	ownerProfileID, err := s.pipelineEntryOwnerProfileID(ctx, entryID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "pipeline_entry_not_found"})
		return
	}
	if err != nil {
		s.logger.Error("pipeline-calendar: entry lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if ownerProfileID != profile.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "pipeline_entry_not_found"})
		return
	}

	entry, err := s.fetchPipelineEntry(ctx, entryID)
	if err != nil {
		s.logger.Error("pipeline-calendar: fetch entry failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	events := s.buildPipelineCalendarEvents(ctx, entry)

	body := renderICS(events)
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pipeline-%s.ics"`, entryID))
	w.Write([]byte(body))
}

// buildPipelineCalendarEvents gathers whichever of the 3 dates are actually
// available — never fabricates a missing one. Errors while looking up the
// optional raw-detail dates are logged but non-fatal: the caller still gets
// an .ics with whatever events were resolved (at minimum, 제출마감).
func (s *Server) buildPipelineCalendarEvents(ctx context.Context, entry *pipelineEntry) []icsEvent {
	events := []icsEvent{}

	if entry.SubmissionDeadline != nil {
		if d, err := time.Parse("2006-01-02", *entry.SubmissionDeadline); err == nil {
			events = append(events, icsEvent{
				uid:             entry.ID + "-submission@biz-platform",
				summary:         "[제출마감] " + entry.NoticeTitle,
				allDay:          true,
				start:           d,
				pipelineEntryID: entry.ID,
				noticeTitle:     entry.NoticeTitle,
				kind:            "submission",
			})
		}
	}

	var currentVersion int
	err := s.db.QueryRowContext(ctx, `SELECT current_version FROM notices WHERE id = $1`, entry.NoticeID).Scan(&currentVersion)
	if err != nil {
		s.logger.Error("pipeline-calendar: notice version lookup failed", "error", err)
		return events
	}
	versionID, err := s.currentVersionID(ctx, entry.NoticeID, currentVersion)
	if err != nil {
		s.logger.Error("pipeline-calendar: current version id lookup failed", "error", err)
		return events
	}
	detail, err := s.fetchNoticeRawDetail(ctx, versionID)
	if err != nil {
		s.logger.Error("pipeline-calendar: raw detail fetch failed", "error", err)
		return events
	}
	if detail == nil {
		return events
	}

	if detail.QualificationDeadlineAt != nil {
		events = append(events, icsEvent{
			uid:             entry.ID + "-qualification@biz-platform",
			summary:         "[등록마감] " + entry.NoticeTitle,
			start:           *detail.QualificationDeadlineAt,
			pipelineEntryID: entry.ID,
			noticeTitle:     entry.NoticeTitle,
			kind:            "qualification",
		})
	}
	if detail.BidOpeningAt != nil {
		events = append(events, icsEvent{
			uid:             entry.ID + "-opening@biz-platform",
			summary:         "[개찰일시] " + entry.NoticeTitle,
			start:           *detail.BidOpeningAt,
			pipelineEntryID: entry.ID,
			noticeTitle:     entry.NoticeTitle,
			kind:            "opening",
		})
	}
	return events
}

// calendarEventItem is the JSON shape GET /api/pipeline/calendar-events
// returns — the인앱 캘린더 화면(월/주/일 뷰)이 쓰는 구조화된 버전. icsEvent
// 그대로 노출하지 않는 이유는 uid/summary가 .ics 포맷 전용 표현이라서다
// (summary엔 "[제출마감] " 같은 접두사가 이미 박혀 있음 — 화면에서는
// kind로 직접 배지를 그리는 쪽이 낫다).
type calendarEventItem struct {
	PipelineEntryID string    `json:"pipelineEntryId"`
	NoticeTitle     string    `json:"noticeTitle"`
	Kind            string    `json:"kind"` // "submission" | "qualification" | "opening"
	AllDay          bool      `json:"allDay"`
	Start           time.Time `json:"start"`
}

// handleListPipelineCalendarEvents — GET /api/pipeline/calendar-events.
// "진행 중"(pipelineActiveStatuses) 파이프라인 전체의 일정을 한 번에
// 모아 인앱 캘린더 화면에 내려준다. 종결된 건(제출완료/낙찰/탈락/보류/
// 제외)은 더 이상 챙길 필요가 없어 제외한다 — dashboard.go의 같은
// 판단과 동일.
func (s *Server) handleListPipelineCalendarEvents(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()

	profile, err := s.getCompanyProfile(r, userID)
	if err != nil {
		s.logger.Error("pipeline-calendar-events: profile lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if profile == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []calendarEventItem{}})
		return
	}

	rows, err := s.db.QueryContext(ctx,
		pipelineEntrySelect+` WHERE pe.company_profile_id = $1`, profile.ID)
	if err != nil {
		s.logger.Error("pipeline-calendar-events: query failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	var entries []pipelineEntry
	for rows.Next() {
		entry, err := scanPipelineEntry(rows)
		if err != nil {
			s.logger.Error("pipeline-calendar-events: scan failed", "error", err)
			continue
		}
		if !pipelineActiveStatuses[entry.Status] {
			continue
		}
		entries = append(entries, *entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Error("pipeline-calendar-events: rows iteration failed", "error", err)
	}

	items := []calendarEventItem{}
	for _, entry := range entries {
		for _, e := range s.buildPipelineCalendarEvents(ctx, &entry) {
			items = append(items, calendarEventItem{
				PipelineEntryID: e.pipelineEntryID,
				NoticeTitle:     e.noticeTitle,
				Kind:            e.kind,
				AllDay:          e.allDay,
				Start:           e.start,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// renderICS builds an RFC 5545 VCALENDAR document. CRLF line endings,
// 75-octet line folding, and TEXT-value escaping are all required by the
// spec for broad compatibility across calendar clients (Google/Outlook/
// Apple all parse strictly here).
func renderICS(events []icsEvent) string {
	var b strings.Builder
	writeLine := func(s string) {
		b.WriteString(foldICSLine(s))
		b.WriteString("\r\n")
	}

	writeLine("BEGIN:VCALENDAR")
	writeLine("VERSION:2.0")
	writeLine("PRODID:-//biz-platform//participation-calendar//KO")
	writeLine("CALSCALE:GREGORIAN")

	now := time.Now().UTC().Format("20060102T150405Z")
	for _, e := range events {
		writeLine("BEGIN:VEVENT")
		writeLine("UID:" + icsEscape(e.uid))
		writeLine("DTSTAMP:" + now)
		writeLine("SUMMARY:" + icsEscape(e.summary))
		if e.allDay {
			writeLine("DTSTART;VALUE=DATE:" + e.start.Format("20060102"))
			writeLine("DTEND;VALUE=DATE:" + e.start.AddDate(0, 0, 1).Format("20060102"))
		} else {
			writeLine("DTSTART:" + e.start.UTC().Format("20060102T150405Z"))
		}
		writeLine("END:VEVENT")
	}

	writeLine("END:VCALENDAR")
	return b.String()
}

// icsEscape escapes the TEXT value special characters RFC 5545 §3.3.11
// requires (backslash, comma, semicolon, newline). SUMMARY/UID here are
// always our own constructed strings (title + fixed labels), but notice
// titles are free text from g2b, so this isn't skippable.
func icsEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `,`, `\,`, `;`, `\;`, "\n", `\n`)
	return r.Replace(s)
}

// foldICSLine wraps a content line at 75 octets per RFC 5545 §3.1, using a
// single leading space to mark continuation lines (which itself counts
// toward that line's 75-octet budget). Splits only on UTF-8 rune boundaries
// so multi-byte Korean text never gets corrupted mid-character.
func foldICSLine(s string) string {
	const maxOctets = 75
	b := []byte(s)
	if len(b) <= maxOctets {
		return s
	}
	var out strings.Builder
	first := true
	for len(b) > 0 {
		limit := maxOctets
		if !first {
			limit = maxOctets - 1
		}
		if limit > len(b) {
			limit = len(b)
		}
		for limit > 0 && limit < len(b) && !utf8.RuneStart(b[limit]) {
			limit--
		}
		if !first {
			out.WriteString("\r\n ")
		}
		out.Write(b[:limit])
		b = b[limit:]
		first = false
	}
	return out.String()
}
