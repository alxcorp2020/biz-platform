package api

// attachment_preview.go — 공고 첨부파일 "서버 프록시 미리보기".
//
// 첨부 목록의 [미리보기] → GET /api/attachments/{id}/preview. 흐름:
//  1) attachments 테이블에서 이 id의 download_url/파일명/해시 조회(임의 URL을 받지 않아
//     SSRF 표면이 "우리가 수집한 출처"로 한정된다).
//  2) SSRF-safe fetch(스킴/호스트 allowlist + 사설·loopback IP 차단 + 리다이렉트 재검증 +
//     크기·타임아웃 상한)로 원본을 서버가 대신 받아온다.
//  3) URL 확장자를 믿지 않고 magic byte(+Content-Type/파일명)로 실제 포맷을 판별한다
//     — 나라장터처럼 확장자 없는 다운로드 URL도 정상 처리.
//  4) PDF면 그대로, 변환 대상(hwp/hwpx/doc/docx/xls/xlsx)이면 상용 변환 API로 PDF 변환.
//  5) 최종 미리보기 포맷은 항상 PDF로 통일해 application/pdf로 인라인 스트림.
//  6) file_hash 기준 로컬 캐시(같은 파일 재변환 방지) + 동시 변환 singleflight.
//
// 키 미설정/변환 실패/미지원/과대/다운로드 실패는 JSON 에러 코드로 폴백 → 프론트가
// "미리보기 대신 다운로드"를 안내한다(기존 다운로드 링크는 그대로 유지).
//
// 배포: 현재 운영 이미지(distroless)는 외부 변환기(LibreOffice)를 못 돌리므로, 변환은
// 프로세스 내부가 아니라 "상용 변환 API"에 위임한다(CONVERT_API_* 환경변수). 키가 없으면
// PDF 미리보기만 동작하고 변환 대상은 다운로드 폴백된다(서버는 죽지 않는다 — 다른 미설정
// 통합과 동일한 "graceful degrade" 패턴).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	previewMaxBytes     = 30 << 20 // 원본 다운로드 상한 30MB(너무 큰 파일은 미리보기 거부)
	previewFetchTimeout = 30 * time.Second
)

// previewAllowedHostSuffixes — 서버가 대신 요청해도 되는 첨부 출처(SSRF allowlist).
// download_url은 우리가 수집해 저장한 값이라 출처가 이미 한정적이지만(나라장터/기업마당
// 등 공공조달), 방어적으로 정부/공식 도메인 suffix만 허용한다.
var previewAllowedHostSuffixes = []string{
	".g2b.go.kr",
	".bizinfo.go.kr",
	".go.kr", // 공공조달 첨부 대부분 정부(.go.kr) 도메인
}

// convertiblePreviewExt — PDF 변환 대상(상용 변환 API에 위임).
var convertiblePreviewExt = map[string]bool{
	"hwp": true, "hwpx": true, "doc": true, "docx": true, "xls": true, "xlsx": true,
}

// previewConvertGroup — 같은 파일(hash)의 동시 변환 요청을 1회로 합친다(N명이 같은
// 첨부 미리보기를 동시에 눌러도 변환 API 호출은 한 번).
var previewConvertGroup singleflight.Group

// previewErr — 프론트 폴백 분기를 위한 코드 있는 에러.
type previewErr struct {
	code   string
	status int
	err    error
}

func (e previewErr) Error() string {
	if e.err != nil {
		return e.code + ": " + e.err.Error()
	}
	return e.code
}

func (s *Server) handleAttachmentPreview(w http.ResponseWriter, r *http.Request) {
	// 로그인 요구 — 서버가 외부 URL을 대신 요청하는 엔드포인트라 익명 남용을 막는다.
	if _, ok := s.currentUserID(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}

	var downloadURL, origName, fileHash string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(download_url,''), original_filename, file_hash
		 FROM attachments WHERE id = $1`, id).Scan(&downloadURL, &origName, &fileHash)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if err != nil {
		s.logger.Error("attachment preview: lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}
	if strings.TrimSpace(downloadURL) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "no_download_url"})
		return
	}

	// 캐시(hash → 변환/원본 PDF) 우선 — 재다운로드·재변환 없이 바로 스트림.
	if pdf, ok := s.previewCacheGet(fileHash); ok {
		s.writePreviewPDF(w, pdf, origName)
		return
	}

	body, ctype, ferr := s.ssrfSafeFetch(r.Context(), downloadURL, previewMaxBytes)
	if ferr != nil {
		s.writePreviewFetchError(w, ferr)
		return
	}

	kind := detectDocKind(body, ctype, downloadURL, origName)
	if kind == "pdf" {
		s.previewCachePut(fileHash, body)
		s.writePreviewPDF(w, body, origName)
		return
	}
	if !convertiblePreviewExt[kind] {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported_format", "kind": kind})
		return
	}

	converter := docConverterFromEnv()
	if converter == nil {
		// 변환 API 키 미설정 — 미리보기 대신 다운로드 폴백(프론트가 안내).
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "conversion_unavailable", "kind": kind})
		return
	}

	// 같은 파일 동시 변환은 1회로 합친다.
	v, cerr, _ := previewConvertGroup.Do(fileHash, func() (interface{}, error) {
		if pdf, ok := s.previewCacheGet(fileHash); ok {
			return pdf, nil
		}
		pdf, e := converter.ToPDF(r.Context(), kind, body, origName)
		if e != nil {
			return nil, e
		}
		s.previewCachePut(fileHash, pdf)
		return pdf, nil
	})
	if cerr != nil {
		s.logger.Error("attachment preview: conversion failed", "kind", kind, "error", cerr)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "conversion_failed"})
		return
	}
	pdf, _ := v.([]byte)
	if len(pdf) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "conversion_failed"})
		return
	}
	s.writePreviewPDF(w, pdf, origName)
}

// writePreviewPDF — 미리보기 PDF를 인라인으로 스트림한다(브라우저 내장 PDF 뷰어가 연다).
func (s *Server) writePreviewPDF(w http.ResponseWriter, pdf []byte, origName string) {
	name := previewPDFName(origName)
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdf)
}

// writePreviewFetchError — SSRF/다운로드 단계 에러를 프론트 분기용 코드로 내려준다.
func (s *Server) writePreviewFetchError(w http.ResponseWriter, err error) {
	var pe previewErr
	if !errors.As(err, &pe) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetch_failed"})
		return
	}
	switch pe.code {
	case "too_large":
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
	case "bad_url", "host_not_allowed", "blocked_ip":
		writeJSON(w, http.StatusForbidden, map[string]string{"error": pe.code})
	default:
		s.logger.Warn("attachment preview: fetch failed", "code", pe.code, "error", pe.err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "fetch_failed"})
	}
}

func previewPDFName(origName string) string {
	base := strings.TrimSpace(origName)
	if base == "" {
		return "preview.pdf"
	}
	if dot := strings.LastIndex(base, "."); dot > 0 {
		base = base[:dot]
	}
	return base + ".pdf"
}

// ---------- SSRF-safe fetch ----------

func previewHostAllowed(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	for _, suf := range previewAllowedHostSuffixes {
		if strings.HasSuffix(h, suf) || h == strings.TrimPrefix(suf, ".") {
			return true
		}
	}
	return false
}

// isBlockedIP — 사설/loopback/링크로컬/멀티캐스트/미지정 등 내부망 주소 차단(SSRF).
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// 100.64.0.0/10 (CGNAT) 등 추가 사설대역.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true
	}
	return false
}

// ssrfSafeFetch — 허용 호스트만, 해석된 IP가 내부망이 아닐 때만 다운로드한다.
func (s *Server) ssrfSafeFetch(ctx context.Context, rawURL string, maxBytes int64) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", previewErr{code: "bad_url", err: err}
	}
	if !previewHostAllowed(u.Hostname()) {
		return nil, "", previewErr{code: "host_not_allowed"}
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, splitErr := net.SplitHostPort(addr)
			if splitErr != nil {
				host = addr
			}
			ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
			if e != nil {
				return nil, e
			}
			for _, ip := range ips {
				if isBlockedIP(ip.IP) {
					return nil, previewErr{code: "blocked_ip"}
				}
			}
			// 호스트 allowlist가 신뢰 정부 도메인으로 한정돼 있어, TLS(SNI/인증서)를
			// 위해 원래 주소로 다이얼한다(사설 IP는 위에서 이미 차단).
			return dialer.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	client := &http.Client{
		Timeout:   previewFetchTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !previewHostAllowed(req.URL.Hostname()) {
				return previewErr{code: "host_not_allowed"}
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", previewErr{code: "bad_url", err: err}
	}
	req.Header.Set("User-Agent", "biz-platform-preview/1.0")
	resp, err := client.Do(req)
	if err != nil {
		var pe previewErr
		if errors.As(err, &pe) {
			return nil, "", pe
		}
		return nil, "", previewErr{code: "fetch_failed", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", previewErr{code: "fetch_status", status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", previewErr{code: "read_failed", err: err}
	}
	if int64(len(body)) > maxBytes {
		return nil, "", previewErr{code: "too_large"}
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// ---------- 실제 포맷 판별(확장자 불신) ----------

// detectDocKind — magic byte 우선, 그다음 확장자/Content-Type. 반환: pdf|hwp|hwpx|doc|
// docx|xls|xlsx|unknown.
func detectDocKind(body []byte, ctype, rawURL, filename string) string {
	ext := previewExtOf(filename, rawURL)

	// 1) 시그니처
	if len(body) >= 4 && bytes.HasPrefix(body, []byte("%PDF")) {
		return "pdf"
	}
	isZip := len(body) >= 4 && body[0] == 0x50 && body[1] == 0x4B &&
		(body[2] == 0x03 || body[2] == 0x05 || body[2] == 0x07)
	isOLE := len(body) >= 8 && bytes.HasPrefix(body, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})

	if isZip {
		if ext == "hwpx" || ext == "docx" || ext == "xlsx" || ext == "pptx" {
			return ext
		}
		return detectZipDocKind(body) // zip 내부 엔트리로 hwpx/docx/xlsx 추정
	}
	if isOLE {
		if ext == "hwp" || ext == "doc" || ext == "xls" {
			return ext
		}
		// HWP 5.x는 OLE 스트림에 "HWP Document File" 시그니처를 담는다.
		if bytes.Contains(body, []byte("HWP Document File")) {
			return "hwp"
		}
		return "unknown"
	}

	// 2) Content-Type
	switch ct := strings.ToLower(ctype); {
	case strings.Contains(ct, "pdf"):
		return "pdf"
	case strings.Contains(ct, "hwpml") || strings.Contains(ct, "hwp+zip"):
		return "hwpx"
	case strings.Contains(ct, "haansofthwp") || strings.Contains(ct, "x-hwp"):
		return "hwp"
	case strings.Contains(ct, "wordprocessingml"):
		return "docx"
	case strings.Contains(ct, "spreadsheetml"):
		return "xlsx"
	case strings.Contains(ct, "msword"):
		return "doc"
	case strings.Contains(ct, "ms-excel"):
		return "xls"
	}

	// 3) 확장자 폴백
	if ext != "" {
		return ext
	}
	return "unknown"
}

// detectZipDocKind — zip(OOXML/HWPX) 내부 중앙 디렉터리에 나타나는 엔트리 이름으로 구분.
func detectZipDocKind(body []byte) string {
	switch {
	case bytes.Contains(body, []byte("application/hwp+zip")) || bytes.Contains(body, []byte("Contents/content.hpf")) || bytes.Contains(body, []byte("version.xml")) && bytes.Contains(body, []byte("hwpml")):
		return "hwpx"
	case bytes.Contains(body, []byte("word/document.xml")):
		return "docx"
	case bytes.Contains(body, []byte("xl/workbook.xml")):
		return "xlsx"
	case bytes.Contains(body, []byte("ppt/presentation.xml")):
		return "pptx"
	}
	return "unknown"
}

// previewExtOf — 파일명 → URL 순으로 확장자를 뽑는다(소문자, 점 제외). 없으면 "".
func previewExtOf(filename, rawURL string) string {
	if e := extLower(filename); e != "" {
		return e
	}
	if u, err := url.Parse(rawURL); err == nil {
		if e := extLower(u.Path); e != "" {
			return e
		}
		// 쿼리스트링에 fileNm=...hwp 같은 실파일명이 실려 오는 경우.
		for _, key := range []string{"fileNm", "fileName", "filename", "orignlFileNm"} {
			if v := u.Query().Get(key); v != "" {
				if e := extLower(v); e != "" {
					return e
				}
			}
		}
	}
	return ""
}

func extLower(name string) string {
	e := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	switch e {
	case "pdf", "hwp", "hwpx", "doc", "docx", "xls", "xlsx", "pptx":
		return e
	}
	return ""
}

// ---------- 해시 기준 로컬 캐시 ----------

func (s *Server) previewCacheDir() string {
	base := strings.TrimSpace(s.attachmentDir)
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "preview-cache")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func previewSafeHash(hash string) string {
	h := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			return r
		}
		return -1
	}, hash)
	if len(h) > 128 {
		h = h[:128]
	}
	return h
}

func (s *Server) previewCachePath(hash string) string {
	return filepath.Join(s.previewCacheDir(), previewSafeHash(hash)+".pdf")
}

func (s *Server) previewCacheGet(hash string) ([]byte, bool) {
	if previewSafeHash(hash) == "" {
		return nil, false
	}
	b, err := os.ReadFile(s.previewCachePath(hash))
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

func (s *Server) previewCachePut(hash string, pdf []byte) {
	if previewSafeHash(hash) == "" || len(pdf) == 0 {
		return
	}
	path := s.previewCachePath(hash)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pdf, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// ---------- 상용 변환 API 클라이언트(CloudConvert, env 게이트) ----------

// documentConverter — 원본 문서 바이트를 PDF로 변환한다. 구현은 외부 상용 API.
type documentConverter interface {
	ToPDF(ctx context.Context, kind string, data []byte, filename string) ([]byte, error)
}

// docConverterFromEnv — CONVERT_API_KEY가 있으면 CloudConvert 클라이언트를, 없으면 nil을 준다.
// nil이면 handler가 "conversion_unavailable"로 폴백(서버는 죽지 않는다 — 컴파일/테스트는 키
// 없이도 그대로 통과).
//
//	CONVERT_API_KEY  : CloudConvert API 키(Bearer). 없으면 변환 비활성.
//	CONVERT_API_BASE : CloudConvert API base. 기본 https://api.cloudconvert.com/v2
//	                   (샌드박스는 https://api.sandbox.cloudconvert.com/v2).
//
// CloudConvert는 HWP·HWPX·DOC·DOCX·XLS·XLSX → PDF를 공식 지원한다(input_format을 그대로 넘긴다).
func docConverterFromEnv() documentConverter {
	key := strings.TrimSpace(os.Getenv("CONVERT_API_KEY"))
	if key == "" {
		return nil
	}
	base := strings.TrimSpace(os.Getenv("CONVERT_API_BASE"))
	if base == "" {
		base = "https://api.cloudconvert.com/v2"
	}
	return &cloudConvertClient{apiKey: key, baseURL: strings.TrimRight(base, "/")}
}

// cloudConvertClient — CloudConvert v2 Job 흐름(import/upload → convert → export/url).
//  1. POST /jobs 로 세 태스크(업로드/변환/내보내기)를 담은 Job 생성 → 업로드 태스크의
//     result.form(멀티파트 업로드 URL+파라미터)을 받는다.
//  2. 그 form URL로 파일 바이트를 멀티파트 업로드(파라미터 먼저, file 필드 마지막).
//  3. GET /jobs/{id}/wait 로 완료까지 대기(조기 반환 대비 폴링).
//  4. export 태스크 result.files[0].url 에서 변환된 PDF를 내려받는다.
type cloudConvertClient struct {
	apiKey  string
	baseURL string
	http    *http.Client // 테스트 주입용(nil이면 기본 클라이언트)
}

// CloudConvert Job/Task 응답(필요 필드만).
type ccJobResp struct {
	Data ccJob `json:"data"`
}
type ccJob struct {
	ID     string   `json:"id"`
	Status string   `json:"status"` // waiting|processing|finished|error
	Tasks  []ccTask `json:"tasks"`
}
type ccTask struct {
	Name      string       `json:"name"`
	Operation string       `json:"operation"`
	Status    string       `json:"status"`
	Message   string       `json:"message"`
	Result    ccTaskResult `json:"result"`
}
type ccTaskResult struct {
	Form  *ccUploadForm `json:"form"`
	Files []ccFile      `json:"files"`
}
type ccUploadForm struct {
	URL        string            `json:"url"`
	Parameters map[string]string `json:"parameters"`
}
type ccFile struct {
	Filename string `json:"filename"`
	URL      string `json:"url"`
}

func (c *cloudConvertClient) client() *http.Client {
	if c.http != nil {
		return c.http
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *cloudConvertClient) ToPDF(ctx context.Context, kind string, data []byte, filename string) ([]byte, error) {
	// 전체 변환 데드라인(브라우저가 무한정 대기하지 않게).
	ctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()

	// 1) Job 생성 — import/upload → convert(input_format=kind, output_format=pdf) → export/url.
	reqBody := map[string]any{
		"tasks": map[string]any{
			"import-1": map[string]any{"operation": "import/upload"},
			"convert-1": map[string]any{
				"operation":     "convert",
				"input":         "import-1",
				"input_format":  kind, // hwp|hwpx|doc|docx|xls|xlsx — CloudConvert 토큰과 동일
				"output_format": "pdf",
			},
			"export-1": map[string]any{"operation": "export/url", "input": "convert-1"},
		},
	}
	var created ccJobResp
	if err := c.doJSON(ctx, http.MethodPost, "/jobs", reqBody, &created); err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	form := ccFindUploadForm(created.Data.Tasks)
	if form == nil || form.URL == "" {
		return nil, errors.New("cloudconvert: no upload form in job")
	}

	// 2) 업로드(멀티파트: 폼 parameters 먼저, file 필드 마지막).
	if err := c.uploadFile(ctx, form, previewSafeFilename(filename, kind), data); err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}

	// 3) 완료 대기.
	job, err := c.waitJob(ctx, created.Data.ID)
	if err != nil {
		return nil, err
	}
	if job.Status == "error" {
		return nil, fmt.Errorf("cloudconvert job error: %s", ccJobErrMessage(job))
	}

	// 4) export 결과 PDF URL → 다운로드.
	fileURL := ccFindExportURL(job.Tasks)
	if fileURL == "" {
		return nil, errors.New("cloudconvert: no export file url")
	}
	pdf, err := c.downloadResult(ctx, fileURL)
	if err != nil {
		return nil, fmt.Errorf("download result: %w", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		return nil, errors.New("cloudconvert: result is not pdf")
	}
	return pdf, nil
}

// doJSON — CloudConvert API(base)로 Bearer 인증 JSON 요청.
func (c *cloudConvertClient) doJSON(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %.300s", resp.StatusCode, string(raw))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// uploadFile — import/upload 태스크가 준 form(URL+parameters)으로 멀티파트 업로드.
// 업로드 URL은 CloudConvert 스토리지(S3 등)라 Bearer 인증이 아니라 form parameters로 인증한다.
func (c *cloudConvertClient) uploadFile(ctx context.Context, form *ccUploadForm, filename string, data []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range form.Parameters {
		_ = mw.WriteField(k, v)
	}
	fw, err := mw.CreateFormFile("file", filename) // file 필드는 파라미터 뒤(마지막)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, form.URL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload status %d", resp.StatusCode)
	}
	return nil
}

// waitJob — GET /jobs/{id}/wait 로 완료까지 대기(서버측 long-poll). 조기 반환/부분완료 대비
// 폴링으로 보강하고, 데드라인/컨텍스트 취소를 존중한다.
func (c *cloudConvertClient) waitJob(ctx context.Context, id string) (*ccJob, error) {
	deadline := time.Now().Add(140 * time.Second)
	for {
		var jr ccJobResp
		err := c.doJSON(ctx, http.MethodGet, "/jobs/"+url.PathEscape(id)+"/wait?include=tasks", nil, &jr)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if jr.Data.Status == "finished" || jr.Data.Status == "error" {
			return &jr.Data, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("cloudconvert: wait timeout")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// downloadResult — export/url 태스크가 준 서명 URL에서 변환된 PDF를 내려받는다(크기 상한).
func (c *cloudConvertClient) downloadResult(ctx context.Context, fileURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, previewMaxBytes+1))
}

// ccFindUploadForm — 태스크 목록에서 import/upload 태스크의 업로드 form을 찾는다.
func ccFindUploadForm(tasks []ccTask) *ccUploadForm {
	for _, t := range tasks {
		if t.Operation == "import/upload" && t.Result.Form != nil {
			return t.Result.Form
		}
	}
	return nil
}

// ccFindExportURL — export/url 태스크의 결과 파일 URL(가장 먼저 발견되는 것)을 반환.
func ccFindExportURL(tasks []ccTask) string {
	for _, t := range tasks {
		if t.Operation == "export/url" {
			for _, f := range t.Result.Files {
				if f.URL != "" {
					return f.URL
				}
			}
		}
	}
	return ""
}

// ccJobErrMessage — error 상태 Job의 실패 사유(가장 먼저 발견되는 실패 태스크 메시지).
func ccJobErrMessage(job *ccJob) string {
	for _, t := range job.Tasks {
		if t.Status == "error" && t.Message != "" {
			return t.Message
		}
	}
	return "unknown"
}

func previewSafeFilename(name, from string) string {
	base := strings.TrimSpace(name)
	if base == "" {
		base = "document." + from
	}
	return base
}
