// cms_images.go — 관리자 CMS(배너/팝업)가 공유하는 이미지 업로드/서빙.
// company_documents.go의 receiveCompanyDocument와 같은 원칙(매직바이트
// 검증, 해시 기반 dedup 저장)을 따르되 별도 함수로 뺐다 — 그쪽은 AI
// 추출용 문서(PDF 포함, AI 분석 쿼터 소진)라 이 용도와 목적이 다르다.
//
// SVG는 의도적으로 허용하지 않는다 — XML 기반이라 http.DetectContentType의
// 매직바이트 검증을 신뢰할 수 없고(스크립트 삽입 가능한 텍스트 포맷),
// 관리자가 올리는 배너/팝업 이미지가 사이트 전체에 노출되는 만큼 XSS
// 위험을 감수할 이유가 없다. 초기 시드 배너(banner-1~3.svg)는 업로드가
// 아니라 코드에 포함된 정적 파일이라 이 제약과 무관하다.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const maxCmsImageBytes = 5 << 20 // 5MB

var allowedCmsImageTypes = map[string]string{
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"webp": "image/webp",
}

// handleUploadCmsImage — POST /api/admin/cms-images(관리자 전용). 배너/팝업
// 등록·수정 폼이 이미지 파일을 먼저 업로드해 URL을 받고, 그 URL을 각자의
// 생성/수정 요청 바디에 실어 보낸다(업로드와 메타데이터 저장을 분리 —
// 이미지 없이 텍스트만 수정할 때 재업로드를 강제하지 않기 위함).
func (s *Server) handleUploadCmsImage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSystemAdmin(w, r); !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxCmsImageBytes+(1<<20))
	if err := r.ParseMultipartForm(maxCmsImageBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_too_large_or_invalid"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_required"})
		return
	}
	defer file.Close()

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(header.Filename), "."))
	mediaType, isAllowedType := allowedCmsImageTypes[ext]
	if !isAllowedType {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_file_type"})
		return
	}

	body, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_read_failed"})
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_empty"})
		return
	}
	if len(body) > maxCmsImageBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_too_large"})
		return
	}
	if detected := http.DetectContentType(body); !companyDocumentContentMatchesType(detected, mediaType) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_content_mismatch"})
		return
	}

	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	storedKey := hash + "." + ext
	dir := filepath.Join(s.attachmentDir, "cms-images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.logger.Error("upload-cms-image: mkdir failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
		return
	}
	path := filepath.Join(dir, storedKey)
	if _, statErr := os.Stat(path); statErr != nil {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			s.logger.Error("upload-cms-image: write failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage_failed"})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"imageUrl": "/cms-images/" + storedKey})
}

// handleServeCmsImage — GET /cms-images/{filename}(공개, 인증 불필요) —
// 배너/팝업은 비로그인 방문자도 봐야 한다. filepath.Base로 경로 조작(../
// 등)을 막는다 — storedKey 자체가 항상 "해시.확장자" 형태라 정상 요청에는
// 영향이 없다.
func (s *Server) handleServeCmsImage(w http.ResponseWriter, r *http.Request) {
	filename := filepath.Base(r.PathValue("filename"))
	path := filepath.Join(s.attachmentDir, "cms-images", filename)
	http.ServeFile(w, r, path)
}
