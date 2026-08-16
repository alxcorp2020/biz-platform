package docx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

// readZipPart — zip 안의 파트를 읽는다(없으면 실패).
func readZipPart(t *testing.T, zr *zip.Reader, name string) string {
	t.Helper()
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open %s: %v", name, err)
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			return string(b)
		}
	}
	t.Fatalf("part %s missing", name)
	return ""
}

// wellFormed — encoding/xml 토큰 파서로 끝까지 읽혀야 한다.
func wellFormed(t *testing.T, name, s string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("%s not well-formed: %v", name, err)
		}
	}
}

// 실제 .docx 생성 → 유효한 OOXML zip인지(필수 파트 존재·XML well-formed) + 제목/
// 표/불릿/[확인 필요]/한글/특수문자 이스케이프/평가기준 순서 유지 확인.
func TestDocument_BuildsValidOOXML(t *testing.T) {
	d := New()
	d.CoreTitle = "제안서 초안"
	d.CoreCreator = "biz-platform"
	d.FooterNote = "본 문서는 초안입니다"
	d.Title("2026년 테스트 사업 제안서")
	d.Centered("제안사: 테스트 주식회사")
	d.PageBreak()
	d.Heading(1, "목차")
	d.TOCField("목차는 문서를 열어 필드를 갱신하면 채워집니다.")
	d.Heading(1, "1. 사업 이해도 (20점)")
	d.Paragraph("본문 <특수문자> & \"인용\" 줄1\n줄2")
	d.Bullets([]string{"추진전략", "단계별 수행방법"})
	d.Heading(1, "2. 수행실적 (20점)")
	d.Table([]string{"사업명", "발주처", "금액"}, [][]string{{"A사업", "B기관", "1,000,000원"}})
	d.Heading(1, "3. 전문인력 (20점)")
	d.Paragraph("[확인 필요: 본 사업에 투입할 책임자와 관련 경력을 입력해 주세요.]")

	b, err := d.Bytes()
	if err != nil {
		t.Fatalf("bytes: %v", err)
	}
	if len(b) < 2 || b[0] != 'P' || b[1] != 'K' {
		t.Fatalf("not a zip (PK) file")
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip open: %v", err)
	}
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml", "word/_rels/document.xml.rels", "word/styles.xml", "word/numbering.xml", "word/footer1.xml", "docProps/core.xml", "docProps/app.xml"} {
		wellFormed(t, name, readZipPart(t, zr, name))
	}
	doc := readZipPart(t, zr, "word/document.xml")
	ct := readZipPart(t, zr, "[Content_Types].xml")
	if !strings.Contains(ct, "wordprocessingml.document.main+xml") {
		t.Fatalf("content types missing main document override")
	}
	for _, want := range []string{"2026년 테스트 사업 제안서", "1. 사업 이해도 (20점)", "2. 수행실적 (20점)", "3. 전문인력 (20점)", "[확인 필요: 본 사업에 투입할 책임자와 관련 경력을 입력해 주세요.]", "추진전략", "1,000,000원", `<w:pStyle w:val="Heading1"/>`, `<w:tbl>`, `<w:numId w:val="1"/>`, `<w:br/>`, `w:type="page"`} {
		if !strings.Contains(doc, want) {
			t.Fatalf("document.xml missing %q", want)
		}
	}
	// 특수문자 이스케이프(원문 '<특수문자>'가 그대로 들어가면 XML이 깨진다).
	if strings.Contains(doc, "<특수문자>") || !strings.Contains(doc, "&lt;특수문자&gt;") {
		t.Fatalf("special characters must be escaped")
	}
	// 평가기준 순서 유지: 제목1이 1→2→3 순서로 등장.
	i1, i2, i3 := strings.Index(doc, "1. 사업 이해도"), strings.Index(doc, "2. 수행실적"), strings.Index(doc, "3. 전문인력")
	if !(i1 < i2 && i2 < i3) {
		t.Fatalf("section order not preserved: %d %d %d", i1, i2, i3)
	}
	// 바닥글 페이지번호 필드 + 안내문.
	ftr := readZipPart(t, zr, "word/footer1.xml")
	if !strings.Contains(ftr, " PAGE ") || !strings.Contains(ftr, "본 문서는 초안입니다") {
		t.Fatalf("footer missing page field/note")
	}
	// styles: 한글 글꼴 지정.
	if !strings.Contains(readZipPart(t, zr, "word/styles.xml"), `w:eastAsia="맑은 고딕"`) {
		t.Fatalf("styles missing eastAsia font")
	}
}
