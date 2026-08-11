package api

import "testing"

func TestMaskOfficerName(t *testing.T) {
	cases := map[string]string{"홍길동": "홍**", "정다혜": "정**", "김철": "김*", "이": "이", "": "", "John Doe": "J*******"}
	for in, want := range cases {
		if got := maskOfficerName(in); got != want {
			t.Errorf("maskOfficerName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestMaskOfficerPhone(t *testing.T) {
	cases := map[string]string{
		"062-1234-5678": "062-****-****",
		"044-320-3242":  "044-***-****",
		"02-123-4567":   "02-***-****",
		"01012345678":   "010********",
		"":              "",
		"12":            "12",
	}
	for in, want := range cases {
		if got := maskOfficerPhone(in); got != want {
			t.Errorf("maskOfficerPhone(%q)=%q want %q", in, got, want)
		}
	}
}

func TestMaskOfficerEmail(t *testing.T) {
	cases := map[string]string{
		"test@example.com":  "te**@example.com",
		"a@b.com":           "a@b.com",     // local 1자 → 그대로
		"ab@b.com":          "ab@b.com",    // local 2자 → 그대로
		"abcd@sejong.go.kr": "ab**@sejong.go.kr",
		"":                  "",
		"noatsign":          "noatsign",
	}
	for in, want := range cases {
		if got := maskOfficerEmail(in); got != want {
			t.Errorf("maskOfficerEmail(%q)=%q want %q", in, got, want)
		}
	}
}

func TestApplyOfficerMasking(t *testing.T) {
	base := func() *noticeRawDetail {
		return &noticeRawDetail{OfficerName: "정다혜", OfficerPhone: "044-320-3242", OfficerEmail: "test@sejong.go.kr"}
	}

	// 1) system_admin → 마스킹 없음, 원본 그대로, 버튼 없음.
	d := base()
	applyOfficerMasking(d, true, false)
	if d.OfficerMasked || d.OfficerCanReveal || d.OfficerName != "정다혜" || d.OfficerPhone != "044-320-3242" {
		t.Errorf("admin should be unmasked: %+v", d)
	}

	// 2) 일반 사용자 + 파이프라인 없음 → 마스킹, 버튼 없음, Full 미전송.
	d = base()
	applyOfficerMasking(d, false, false)
	if !d.OfficerMasked || d.OfficerCanReveal {
		t.Errorf("non-pipeline should be masked without reveal: %+v", d)
	}
	if d.OfficerName != "정**" || d.OfficerPhone != "044-***-****" || d.OfficerEmail != "te**@sejong.go.kr" {
		t.Errorf("masked values wrong: %q %q %q", d.OfficerName, d.OfficerPhone, d.OfficerEmail)
	}
	if d.OfficerFullName != "" || d.OfficerFullPhone != "" || d.OfficerFullEmail != "" {
		t.Errorf("non-pipeline must NOT receive full PII: %+v", d)
	}

	// 3) 일반 사용자 + 파이프라인 있음 → 마스킹 표시 + 버튼 + Full 전송(공개용).
	d = base()
	applyOfficerMasking(d, false, true)
	if !d.OfficerMasked || !d.OfficerCanReveal {
		t.Errorf("pipeline user should be masked+revealable: %+v", d)
	}
	if d.OfficerName != "정**" {
		t.Errorf("display should still be masked by default: %q", d.OfficerName)
	}
	if d.OfficerFullName != "정다혜" || d.OfficerFullPhone != "044-320-3242" || d.OfficerFullEmail != "test@sejong.go.kr" {
		t.Errorf("pipeline user should receive full for reveal: %+v", d)
	}
}
