package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 확장자 없이도 magic byte로 실제 포맷을 판별하는지(나라장터 다운로드 URL 대응) 검증.
func TestDetectDocKind(t *testing.T) {
	pdf := []byte("%PDF-1.7\n...")
	zipDocx := append([]byte{0x50, 0x4B, 0x03, 0x04}, []byte("....word/document.xml....")...)
	zipXlsx := append([]byte{0x50, 0x4B, 0x03, 0x04}, []byte("....xl/workbook.xml....")...)
	zipHwpx := append([]byte{0x50, 0x4B, 0x03, 0x04}, []byte("mimetypeapplication/hwp+zip....")...)
	oleHwp := append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, []byte("....HWP Document File v5.00....")...)

	cases := []struct {
		name     string
		body     []byte
		ctype    string
		url      string
		filename string
		want     string
	}{
		// 확장자 없는 g2b 스타일 URL + 시그니처만으로 판별
		{"pdf by signature, no ext", pdf, "application/octet-stream", "https://www.g2b.go.kr/pn/downloadFile.do?fileSeq=1", "", "pdf"},
		{"docx by zip entry, no ext", zipDocx, "application/octet-stream", "https://x.go.kr/d?f=1", "", "docx"},
		{"xlsx by zip entry, no ext", zipXlsx, "application/octet-stream", "https://x.go.kr/d?f=1", "", "xlsx"},
		{"hwpx by zip mimetype, no ext", zipHwpx, "application/octet-stream", "https://x.go.kr/d?f=1", "", "hwpx"},
		{"hwp by OLE signature, no ext", oleHwp, "application/octet-stream", "https://x.go.kr/d?f=1", "", "hwp"},
		// 확장자 우선(시그니처가 zip/OLE일 때 확장자로 세부 구분)
		{"hwpx zip + ext", append([]byte{0x50, 0x4B, 0x03, 0x04}, []byte("junk")...), "", "", "과업.hwpx", "hwpx"},
		// Content-Type 폴백(시그니처 애매할 때)
		{"pdf by content-type", []byte("garbage"), "application/pdf", "", "", "pdf"},
		// 확장자 폴백(시그니처/CT 없음)
		{"xlsx by ext only", []byte("garbage"), "", "", "공내역서.xlsx", "xlsx"},
		{"unknown", []byte("garbage"), "text/html", "https://x.go.kr/", "", "unknown"},
	}
	for _, c := range cases {
		if got := detectDocKind(c.body, c.ctype, c.url, c.filename); got != c.want {
			t.Errorf("%s: detectDocKind=%q want %q", c.name, got, c.want)
		}
	}
}

func TestPreviewHostAllowed(t *testing.T) {
	allow := []string{"www.g2b.go.kr", "g2b.go.kr", "www.bizinfo.go.kr", "data.go.kr"}
	for _, h := range allow {
		if !previewHostAllowed(h) {
			t.Errorf("host %q should be allowed", h)
		}
	}
	deny := []string{"", "evil.com", "g2b.go.kr.evil.com", "localhost", "127.0.0.1", "g2b.go.kr.attacker.io"}
	for _, h := range deny {
		if previewHostAllowed(h) {
			t.Errorf("host %q should be denied", h)
		}
	}
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "169.254.1.1", "0.0.0.0", "100.64.0.1", "::1"}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("ip %q should be blocked", s)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "211.237.0.1"} // 211.x = g2b 대역 예시
	for _, s := range public {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("ip %q should be allowed", s)
		}
	}
	if !isBlockedIP(nil) {
		t.Error("nil IP should be blocked")
	}
}

func TestPreviewExtOf(t *testing.T) {
	cases := []struct{ filename, url, want string }{
		{"과업지시서.hwp", "", "hwp"},                                                 // 파일명 확장자 우선
		{"", "https://x.go.kr/a/b/file.pdf", "pdf"},                            // URL 경로 확장자
		{"", "https://www.g2b.go.kr/downloadFile.do?fileNm=notice.xlsx", "xlsx"}, // 쿼리 fileNm 확장자
		{"", "https://x.go.kr/download?id=1", ""},                              // 확장자 없음
	}
	for _, c := range cases {
		if got := previewExtOf(c.filename, c.url); got != c.want {
			t.Errorf("previewExtOf(%q,%q)=%q want %q", c.filename, c.url, got, c.want)
		}
	}
}

// TestCloudConvertToPDF_MockFlow — 실제 CloudConvert 없이 Job/import/upload/wait/export/
// download 흐름 전체를 모의 서버로 검증한다(프로토콜 준수 확인). 실제 CloudConvert 키가
// 없어 실변환은 미검증이지만, 우리 클라이언트가 CloudConvert v2 사양대로 말하는지는 검증됨.
func TestCloudConvertToPDF_MockFlow(t *testing.T) {
	var srvURL string
	var gotInputFormat, gotOutputFormat string
	uploadHit := false

	mux := http.NewServeMux()
	// 1) Job 생성 → 업로드 form 반환. convert 태스크의 input/output_format을 캡처.
	mux.HandleFunc("POST /v2/jobs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tasks map[string]map[string]any `json:"tasks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if c, ok := body.Tasks["convert-1"]; ok {
			gotInputFormat, _ = c["input_format"].(string)
			gotOutputFormat, _ = c["output_format"].(string)
		}
		if r.Header.Get("Authorization") != "Bearer testkey" {
			t.Errorf("missing bearer auth on create job: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"id":"job1","status":"waiting","tasks":[
			{"name":"import-1","operation":"import/upload","status":"waiting","result":{"form":{"url":"`+srvURL+`/upload","parameters":{"key":"abc123"}}}},
			{"name":"convert-1","operation":"convert","status":"waiting"},
			{"name":"export-1","operation":"export/url","status":"waiting"}
		]}}`)
	})
	// 2) 업로드 — 파라미터+file 멀티파트 수신.
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		uploadHit = true
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("upload not multipart: %v", err)
		}
		if r.FormValue("key") != "abc123" {
			t.Errorf("upload missing form parameter key: %q", r.FormValue("key"))
		}
		if _, _, err := r.FormFile("file"); err != nil {
			t.Errorf("upload missing file field: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	})
	// 3) 완료 대기 → finished + export 파일 URL.
	mux.HandleFunc("GET /v2/jobs/job1/wait", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"id":"job1","status":"finished","tasks":[
			{"name":"export-1","operation":"export/url","status":"finished","result":{"files":[{"filename":"out.pdf","url":"`+srvURL+`/result.pdf"}]}}
		]}}`)
	})
	// 4) 결과 PDF 다운로드.
	mux.HandleFunc("GET /result.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, "%PDF-1.4\nmock-converted\n%%EOF")
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	c := &cloudConvertClient{apiKey: "testkey", baseURL: srv.URL + "/v2", http: srv.Client()}
	pdf, err := c.ToPDF(context.Background(), "hwp", []byte("\xd0\xcf\x11\xe0fake-hwp"), "과업지시서.hwp")
	if err != nil {
		t.Fatalf("ToPDF: %v", err)
	}
	if !strings.HasPrefix(string(pdf), "%PDF") {
		t.Errorf("result not pdf: %.20q", string(pdf))
	}
	if gotInputFormat != "hwp" {
		t.Errorf("input_format=%q want hwp (HWP를 CloudConvert에 정확히 넘겨야 함)", gotInputFormat)
	}
	if gotOutputFormat != "pdf" {
		t.Errorf("output_format=%q want pdf", gotOutputFormat)
	}
	if !uploadHit {
		t.Error("upload step was not called")
	}
}
