package api

import (
	"database/sql"
	"testing"
	"time"
)

func TestClassifyNoticeDocRequirements(t *testing.T) {
	lic, cert, dir, track := classifyNoticeDocRequirements([]string{
		"직접생산확인증명서", "실적증명서 1부", "정보통신공사업 면허증 사본", "ISO9001 인증서", "품질경영 인증서", "사업자등록증",
	})
	if !dir {
		t.Errorf("직접생산 요구 감지 실패")
	}
	if !track {
		t.Errorf("실적 요구 감지 실패")
	}
	if len(lic) != 1 || lic[0] != "정보통신공사업 면허증 사본" {
		t.Errorf("면허 요구 감지 실패: %v", lic)
	}
	if len(cert) != 2 {
		t.Errorf("인증 요구 감지 실패(ISO+인증서 2건 기대): %v", cert)
	}
	// 사업자등록증 등 무관 서류는 어떤 카테고리도 만들지 않는다.
}

func TestClassifyNoticeDocRequirements_Empty(t *testing.T) {
	lic, cert, dir, track := classifyNoticeDocRequirements([]string{"사업자등록증", "법인등기부등본", ""})
	if len(lic) != 0 || len(cert) != 0 || dir || track {
		t.Errorf("무관 서류에서 요구조건이 잘못 생성됨: lic=%v cert=%v dir=%v track=%v", lic, cert, dir, track)
	}
}

func TestLicenseCertGapState(t *testing.T) {
	future := sql.NullTime{Time: time.Now().AddDate(1, 0, 0), Valid: true}
	past := sql.NullTime{Time: time.Now().AddDate(-1, 0, 0), Valid: true}
	c := companyEligibilityContext{licenseCertByName: map[string]licenseCertState{
		"정보통신공사업 면허": {status: "보유", expiresAt: future},
		"만료면허":       {status: "보유", expiresAt: past},
		"미보유면허":      {status: "미보유"},
		"모름면허":       {status: "확인되지않음"},
	}}
	cases := []struct {
		name, want string
	}{
		{"정보통신공사업 면허", gapMet}, // 보유·유효 → 충족
		{"만료면허", gapNotMet},    // 보유·만료 → 미충족(PASS 금지)
		{"미보유면허", gapNotMet},   // 답했으나 미보유 → 미충족(회사정보 부족 아님)
		{"모름면허", gapNotMet},    // 확인되지않음(행 존재) → 미충족
		{"등록안한면허", gapCompany}, // 행 자체 없음 → 회사정보 부족
	}
	for _, tc := range cases {
		if got := c.licenseCertGapState(tc.name); got != tc.want {
			t.Errorf("licenseCertGapState(%q)=%q want %q", tc.name, got, tc.want)
		}
	}
}

func TestAnyCompanyGap(t *testing.T) {
	c := companyEligibilityContext{licenseCertByName: map[string]licenseCertState{
		"보유면허": {status: "보유"},
	}}
	if c.anyCompanyGap([]string{"보유면허"}) {
		t.Errorf("보유 면허만 요구인데 company_gap으로 판정됨")
	}
	if !c.anyCompanyGap([]string{"보유면허", "미등록면허"}) {
		t.Errorf("미등록 면허가 섞였는데 company_gap 미감지")
	}
}
