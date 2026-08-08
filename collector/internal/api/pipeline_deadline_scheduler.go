// pipeline_deadline_scheduler.go — Phase B+ (2026-08-09). 시간단위 마감 자동화.
//
// 목표: 사용자가 마감시간을 직접 챙기지 않아도 되게 한다. 사용자 노출 상태는
// 계속 6개(검토중/준비중/제출완료/낙찰/탈락/제외)만 유지한다 — 시간 알림 때문에
// 새 상태를 만들지 않는다. 알림은 단순 "N시간 남음"이 아니라 현재 준비율과 남은
// 업무를 함께 계산한 "상황 기반" 문구다.
//
// 대상: status='준비중' 엔트리(실제 제출 준비 단계). 두 종류의 마감을 본다.
//   - 참가자격등록 마감(notices.qualification_deadline_at): D-3 / D-1 / H-6
//   - 제출마감(notices.application_end_datetime, 없으면 application_end_at 폴백):
//     D-7 / D-3 / D-1 / H-6 / H-2 (H-6/H-2는 시각 데이터가 있을 때만)
//
// dedup은 메모리가 아니라 DB(pipeline_deadline_events)로 한다 — 서버 재시작
// 후에도 같은 이벤트를 다시 보내지 않는다. 이벤트 키는 (엔트리, event_type,
// deadline_at)이라 공고 정정으로 마감시각이 바뀌면 새 날짜 기준으로 다시
// 계산되고, 과거 마감 기준으로 이미 보낸 건은 재발송되지 않는다.
//
// 소급방지: 이벤트 예정시각 event_time은 [floor, now] 구간에 들어와야 발송한다.
//   floor = max(엔트리 참여시각, 마감 인지시각(seen_at), now-24h)
//   - 참여시각보다 이전 이벤트(예: D-1에 참여 → D-3는 이미 지남)는 안 보낸다.
//   - 공고 정정 시 seen_at을 정정 시점으로 갱신 → 새 마감 기준의 "이미 지난"
//     이벤트는 안 보내고 미래 이벤트만 살린다.
//   - now-24h 하한은 배포/장기 다운타임 후 옛 이벤트를 무더기로 소급발송하지
//     않게 하는 안전장치다(정상 운영 시 30분 주기라 항상 이 안에서 발송됨).
package api

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/lib/pq"
)

// 마감 이벤트 종류 키(dedup 원장·notification_log·in-app 공통 event_type).
const (
	evtQualificationD3 = "QUALIFICATION_D3"
	evtQualificationD1 = "QUALIFICATION_D1"
	evtQualificationH6 = "QUALIFICATION_H6"

	evtSubmissionD7 = "SUBMISSION_D7"
	evtSubmissionD3 = "SUBMISSION_D3"
	evtSubmissionD1 = "SUBMISSION_D1"
	evtSubmissionH6 = "SUBMISSION_H6"
	evtSubmissionH2 = "SUBMISSION_H2"

	evtSubmissionDeadlineChanged    = "SUBMISSION_DEADLINE_CHANGED"
	evtQualificationDeadlineChanged = "QUALIFICATION_DEADLINE_CHANGED"

	// 소급발송 하한(장기 다운타임/배포 후 옛 이벤트 무더기 발송 방지).
	deadlineEventGrace = 24 * time.Hour
)

// deadlineEventDef — 한 이벤트 종류의 정의. offset은 마감시각으로부터 얼마나
// 앞선 시점에 알릴지(양수). kind는 "제출"/"자격". requiresTime이면 시각 데이터가
// 있을 때만(제출마감의 H-6/H-2) 발송한다 — 날짜만 아는 폴백에선 시간단위 알림이
// 무의미하기 때문.
type deadlineEventDef struct {
	eventType    string
	kind         string // "submission" | "qualification"
	offset       time.Duration
	label        string // "7일" / "3일" / "1일" / "6시간" / "2시간"
	tag          string // 알림 제목 태그: "제출마감 D-7" / "제출마감 6시간 전"
	requiresTime bool
}

var submissionEventDefs = []deadlineEventDef{
	{evtSubmissionD7, "submission", 7 * 24 * time.Hour, "7일", "제출마감 D-7", false},
	{evtSubmissionD3, "submission", 3 * 24 * time.Hour, "3일", "제출마감 D-3", false},
	{evtSubmissionD1, "submission", 1 * 24 * time.Hour, "1일", "제출마감 D-1", false},
	{evtSubmissionH6, "submission", 6 * time.Hour, "6시간", "제출마감 6시간 전", true},
	{evtSubmissionH2, "submission", 2 * time.Hour, "2시간", "제출마감 2시간 전", true},
}

var qualificationEventDefs = []deadlineEventDef{
	{evtQualificationD3, "qualification", 3 * 24 * time.Hour, "3일", "자격마감 D-3", false},
	{evtQualificationD1, "qualification", 1 * 24 * time.Hour, "1일", "자격마감 D-1", false},
	{evtQualificationH6, "qualification", 6 * time.Hour, "6시간", "자격마감 6시간 전", false},
}

// deadlineScheduleRow — 준비중 엔트리 한 건의 스케줄 계산에 필요한 필드.
type deadlineScheduleRow struct {
	entryID, noticeID, profileID string
	title                        string
	org                          sql.NullString
	createdAt                    time.Time
	subDeadline                  sql.NullTime // COALESCE(application_end_datetime, application_end_at@KST)
	subHasTime                   bool         // application_end_datetime IS NOT NULL
	qualDeadline                 sql.NullTime
	subSnapshot, subSeenAt       sql.NullTime
	qualSnapshot, qualSeenAt     sql.NullTime
}

// RunDeadlineSchedule은 30분 티커/관리자 수동트리거가 호출하는 진입점.
// 실제 로직은 시각 주입 가능한 runDeadlineScheduleAt에 있다(테스트용).
func (s *Server) RunDeadlineSchedule(ctx context.Context) error {
	return s.runDeadlineScheduleAt(ctx, time.Now())
}

func (s *Server) runDeadlineScheduleAt(ctx context.Context, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pe.id, pe.notice_id, pe.company_profile_id, n.title, n.organization_name, pe.created_at,
		       COALESCE(n.application_end_datetime, (n.application_end_at::timestamp AT TIME ZONE 'Asia/Seoul')) AS sub_deadline,
		       (n.application_end_datetime IS NOT NULL) AS sub_has_time,
		       n.qualification_deadline_at,
		       pe.submission_deadline_snapshot, pe.submission_deadline_seen_at,
		       pe.qualification_deadline_snapshot, pe.qualification_deadline_seen_at
		FROM notice_pipeline_entries pe
		JOIN notices n ON n.id = pe.notice_id
		WHERE pe.status = '준비중'`)
	if err != nil {
		return err
	}
	var entries []deadlineScheduleRow
	for rows.Next() {
		var r deadlineScheduleRow
		if err := rows.Scan(&r.entryID, &r.noticeID, &r.profileID, &r.title, &r.org, &r.createdAt,
			&r.subDeadline, &r.subHasTime, &r.qualDeadline,
			&r.subSnapshot, &r.subSeenAt, &r.qualSnapshot, &r.qualSeenAt); err != nil {
			continue
		}
		entries = append(entries, r)
	}
	if cerr := rows.Err(); cerr != nil {
		rows.Close()
		return cerr
	}
	rows.Close()
	if len(entries) == 0 {
		return nil
	}

	// 준비율(필요서류 대비 '보유') 일괄 조회 — sendDeadlineReminders와 동일 기준.
	prep := s.fetchPrepCounts(ctx, entries)

	for _, e := range entries {
		// 1) 마감 정정 감지 + 스냅샷/seen_at 갱신. 이번 틱에 쓸 유효 seen_at을 반환.
		subSeen := s.reconcileDeadlineSnapshot(ctx, e, now, "submission")
		qualSeen := s.reconcileDeadlineSnapshot(ctx, e, now, "qualification")

		// 2) 제출마감 이벤트
		if e.subDeadline.Valid {
			for _, def := range submissionEventDefs {
				if def.requiresTime && !e.subHasTime {
					continue // 시각 미상이면 시간단위 알림은 건너뜀
				}
				s.maybeFireDeadlineEvent(ctx, e, def, e.subDeadline.Time, subSeen, now, prep[e.entryID])
			}
		}
		// 3) 참가자격등록 마감 이벤트
		if e.qualDeadline.Valid {
			for _, def := range qualificationEventDefs {
				s.maybeFireDeadlineEvent(ctx, e, def, e.qualDeadline.Time, qualSeen, now, prep[e.entryID])
			}
		}
	}
	return nil
}

// prepCount — 한 엔트리의 필요서류 총계/준비완료 수.
type prepCount struct{ total, prepared int }

func (s *Server) fetchPrepCounts(ctx context.Context, entries []deadlineScheduleRow) map[string]prepCount {
	out := map[string]prepCount{}
	if len(entries) == 0 {
		return out
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.entryID
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT pipeline_entry_id::text, count(*), count(*) FILTER (WHERE status = '보유')
		 FROM pipeline_checklist_items WHERE pipeline_entry_id::text = ANY($1) GROUP BY 1`, pq.Array(ids))
	if err != nil {
		s.logger.Error("deadline scheduler: prep count query failed", "error", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c prepCount
		if err := rows.Scan(&id, &c.total, &c.prepared); err == nil {
			out[id] = c
		}
	}
	return out
}

// reconcileDeadlineSnapshot — 이 엔트리의 특정 마감 종류에 대해 스냅샷과 현재
// 공고 마감을 비교한다. 처음 관측이면 스냅샷을 심고(seen_at=참여시각, 알림 없음),
// 값이 바뀌었으면 "마감일 변경" 알림을 보내고 seen_at을 now로 갱신한다.
// 반환값은 이번 틱의 소급방지 floor 계산에 쓸 유효 seen_at.
func (s *Server) reconcileDeadlineSnapshot(ctx context.Context, e deadlineScheduleRow, now time.Time, kind string) time.Time {
	var cur, snapshot, seenAt sql.NullTime
	var snapCol, seenCol, changedEvent, label string
	if kind == "submission" {
		cur, snapshot, seenAt = e.subDeadline, e.subSnapshot, e.subSeenAt
		snapCol, seenCol = "submission_deadline_snapshot", "submission_deadline_seen_at"
		changedEvent, label = evtSubmissionDeadlineChanged, "제출 마감일"
	} else {
		cur, snapshot, seenAt = e.qualDeadline, e.qualSnapshot, e.qualSeenAt
		snapCol, seenCol = "qualification_deadline_snapshot", "qualification_deadline_seen_at"
		changedEvent, label = evtQualificationDeadlineChanged, "참가자격 등록 마감일"
	}

	// 현재 마감이 없으면(미상) 아무것도 안 함 — NULL을 정정으로 보지 않는다.
	if !cur.Valid {
		if seenAt.Valid {
			return seenAt.Time
		}
		return e.createdAt
	}

	switch {
	case !snapshot.Valid:
		// 최초 관측: 스냅샷을 심고 seen_at=참여시각(정상적으로 참여 이후 이벤트가
		// 발송되도록). 배포 시 옛 엔트리 무더기 발송은 now-24h 하한이 막는다.
		s.updateDeadlineSnapshot(ctx, e.entryID, snapCol, seenCol, cur.Time, e.createdAt)
		return e.createdAt
	case !cur.Time.Equal(snapshot.Time):
		// 정정 감지: 사용자 알림 + seen_at=now로 갱신(이후 새 마감 기준의 "이미
		// 지난" 이벤트는 막고 미래 이벤트만 살림). 게이트(deadline_at)가 달라져
		// 새 날짜 기준 이벤트는 자연히 새로 계산된다.
		title := fmt.Sprintf("%s이 변경되었습니다 · %s", label, e.title)
		body := fmt.Sprintf("%s이 %s(으)로 변경되었습니다. 관련 일정과 알림을 자동으로 업데이트했습니다.",
			label, cur.Time.In(kstLocation()).Format("2006-01-02 15:04"))
		if err := s.insertEntryScopedInAppNotification(ctx, e.profileID, changedEvent, e.entryID, e.noticeID, title, body); err != nil {
			s.logger.Error("deadline scheduler: change notice insert failed", "error", err)
		}
		s.sendPushToProfileMembers(ctx, e.profileID, title, body, "/#/pipeline/"+e.entryID)
		s.updateDeadlineSnapshot(ctx, e.entryID, snapCol, seenCol, cur.Time, now)
		return now
	default:
		if seenAt.Valid {
			return seenAt.Time
		}
		return e.createdAt
	}
}

func (s *Server) updateDeadlineSnapshot(ctx context.Context, entryID, snapCol, seenCol string, snapshot, seenAt time.Time) {
	// 컬럼명은 내부 상수(호출부 하드코딩)라 인젝션 위험 없음.
	q := fmt.Sprintf(`UPDATE notice_pipeline_entries SET %s=$2, %s=$3, updated_at=updated_at WHERE id=$1`, snapCol, seenCol)
	if _, err := s.db.ExecContext(ctx, q, entryID, snapshot, seenAt); err != nil {
		s.logger.Error("deadline scheduler: snapshot update failed", "error", err)
	}
}

// maybeFireDeadlineEvent — 한 (엔트리, 이벤트정의, 마감시각) 조합이 지금 발송
// 대상인지 판정하고, 맞으면 DB 게이트를 잠근 뒤(최초 1회만 성공) 알림을 보낸다.
func (s *Server) maybeFireDeadlineEvent(ctx context.Context, e deadlineScheduleRow, def deadlineEventDef, deadlineAt, seenAt, now time.Time, prep prepCount) {
	eventTime := deadlineAt.Add(-def.offset)
	// 소급방지 floor = max(참여시각, seen_at, now-24h).
	floor := e.createdAt
	if seenAt.After(floor) {
		floor = seenAt
	}
	if grace := now.Add(-deadlineEventGrace); grace.After(floor) {
		floor = grace
	}
	// 발송창: floor <= eventTime <= now.
	if eventTime.Before(floor) || eventTime.After(now) {
		return
	}

	// DB 게이트 — (엔트리, event_type, deadline_at) 최초 1회만 INSERT 성공.
	var gated bool
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO pipeline_deadline_events (pipeline_entry_id, notice_id, company_profile_id, event_type, deadline_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (pipeline_entry_id, event_type, deadline_at) DO NOTHING
		RETURNING true`, e.entryID, e.noticeID, e.profileID, def.eventType, deadlineAt).Scan(&gated)
	if err == sql.ErrNoRows {
		return // 이미 발송됨
	}
	if err != nil {
		s.logger.Error("deadline scheduler: gate insert failed", "error", err, "event", def.eventType, "entry", e.entryID)
		return
	}

	s.dispatchDeadlineEvent(ctx, e, def, deadlineAt, now, prep)
}

// dispatchDeadlineEvent — 실제 채널 발송(인앱/푸시/이메일/SMS). 게이트를 이미
// 통과했으므로 여기선 중복판정을 하지 않는다(게이트가 유일한 dedup 기준).
func (s *Server) dispatchDeadlineEvent(ctx context.Context, e deadlineScheduleRow, def deadlineEventDef, deadlineAt, now time.Time, prep prepCount) {
	entryID, noticeID := e.entryID, e.noticeID
	// 상황 기반 본문: 헤더(무슨 마감/얼마 남음) + 준비율/남은업무 + (충돌 시) 자격 우선 안내.
	var head string
	if def.kind == "submission" {
		head = fmt.Sprintf("제출 마감까지 %s 남았습니다.", def.label)
	} else {
		head = fmt.Sprintf("참가자격 등록 마감까지 %s 남았습니다.", def.label)
	}
	prepLine := prepStatusLine(prep)
	conflict := s.deadlineConflictLine(e, def, now)

	bodyParts := []string{head}
	if conflict != "" {
		bodyParts = append(bodyParts, conflict)
	}
	if prepLine != "" {
		bodyParts = append(bodyParts, prepLine)
	}
	body := strings.Join(bodyParts, " ")

	title := fmt.Sprintf("[%s] %s", def.tag, e.title)

	if err := s.insertEntryScopedInAppNotification(ctx, e.profileID, def.eventType, entryID, noticeID, title, body); err != nil {
		s.logger.Error("deadline scheduler: in-app insert failed", "error", err)
	}
	s.sendPushToProfileMembers(ctx, e.profileID, title, body, "/#/pipeline/"+entryID)

	contacts, err := s.fetchNotifiableContacts(ctx, e.profileID, def.eventType, entryID)
	if err != nil {
		s.logger.Error("deadline scheduler: contact lookup failed", "error", err)
		return
	}
	smsAllowed := s.smsAllowedForPlan(ctx, e.profileID)
	emailAllowed := s.checkEmailNotificationQuota(ctx, e.profileID)
	for _, c := range contacts {
		contactID := c.id
		// 게이트가 dedup을 보장하므로 contact별 alreadySent는 보지 않는다.
		if c.emailEnabled && c.email != "" {
			subject := title
			if emailAllowed {
				emailBody := "<p>" + html.EscapeString(body) + "</p><p><b>" + html.EscapeString(e.title) + "</b></p>"
				if e.org.Valid && strings.TrimSpace(e.org.String) != "" {
					emailBody += "<p>발주기관: " + html.EscapeString(e.org.String) + "</p>"
				}
				s.sendNotificationEmail(ctx, def.eventType, c.email, nil, &contactID, &entryID, &noticeID, subject, emailBody)
			} else {
				s.logSkippedEmailNotification(ctx, def.eventType, c.email, nil, &contactID, &entryID, &noticeID, subject)
			}
		}
		if smsAllowed && c.smsEnabled && c.phone != "" {
			msg := fmt.Sprintf("[%s] %s %s", def.tag, truncateForSMS(e.title, 20), head)
			s.sendNotificationSMS(ctx, def.eventType, c.phone, nil, &contactID, &entryID, &noticeID, msg)
		}
	}
}

// prepStatusLine — 준비율 문구. 체크리스트가 없으면 빈 문자열(문구 생략).
func prepStatusLine(p prepCount) string {
	if p.total == 0 {
		return ""
	}
	remaining := p.total - p.prepared
	if remaining <= 0 {
		return "준비는 모두 완료됐습니다. 나라장터 최종 제출 여부를 확인해주세요."
	}
	return fmt.Sprintf("필요서류 %d개 중 %d개가 준비됐습니다. 남은 업무 %d개를 확인해주세요.", p.total, p.prepared, remaining)
}

// deadlineConflictLine — 참가자격 마감이 제출마감보다 먼저 오는 경우, 제출 관련
// 알림에 "자격 등록이 먼저 마감된다"는 우선 안내를 붙인다(자격 알림 자체엔
// 붙이지 않는다 — 이미 자격 마감을 다루고 있으므로).
func (s *Server) deadlineConflictLine(e deadlineScheduleRow, def deadlineEventDef, now time.Time) string {
	if def.kind != "submission" {
		return ""
	}
	if !e.qualDeadline.Valid || !e.subDeadline.Valid {
		return ""
	}
	if e.qualDeadline.Time.After(now) && e.qualDeadline.Time.Before(e.subDeadline.Time) {
		return "입찰 제출보다 먼저 참가자격 등록이 마감됩니다."
	}
	return ""
}

var _kstLoc *time.Location

func kstLocation() *time.Location {
	if _kstLoc == nil {
		if loc, err := time.LoadLocation("Asia/Seoul"); err == nil {
			_kstLoc = loc
		} else {
			_kstLoc = time.FixedZone("KST", 9*3600)
		}
	}
	return _kstLoc
}
