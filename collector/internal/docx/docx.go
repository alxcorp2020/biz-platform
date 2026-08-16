// Package docx — 의존성 없는 최소 DOCX(OOXML WordprocessingML) 생성기(2026-08-16,
// 평가기준 맞춤 제안서). 표준 라이브러리 archive/zip + encoding/xml만 쓴다.
//
// 목적은 "Word/한글 호환 워드프로세서에서 열어 수정할 수 있는 실제 .docx"다 —
// 표지·제목 계층(제목1/2/3)·본문·불릿·표·페이지나누기·바닥글 페이지번호를
// 지원하고 그 이상(이미지/각주/추적변경)은 만들지 않는다. 스타일은 공공 제출용
// 문서 느낌으로 절제(A4, 맑은 고딕/기본 글꼴 지정, 굵은 제목, 회색 표 머리글).
// 한글 글꼴은 이름만 지정한다(w:rFonts eastAsia="맑은 고딕") — 서버에 글꼴 파일이
// 없어도 되고, 열어보는 쪽 워드프로세서가 없으면 대체 글꼴로 표시된다.
package docx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// Document — 순서대로 쌓이는 블록 목록.
type Document struct {
	blocks []string
	// FooterNote — 바닥글 왼쪽 작은 안내문(예: "제출 전 직접 확인" 문구). 페이지
	// 번호는 오른쪽에 항상 붙는다.
	FooterNote string
	// CoreTitle/CoreCreator — docProps/core.xml 메타데이터(선택).
	CoreTitle   string
	CoreCreator string
}

func New() *Document { return &Document{} }

// esc — XML 텍스트 이스케이프.
func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// run — 하나의 텍스트 런. xml:space="preserve"로 앞뒤 공백 보존, 줄바꿈은 <w:br/>.
func run(text string, bold bool, sizeHalfPt int, color string) string {
	var rpr strings.Builder
	if bold {
		rpr.WriteString(`<w:b/><w:bCs/>`)
	}
	if color != "" {
		rpr.WriteString(`<w:color w:val="` + color + `"/>`)
	}
	if sizeHalfPt > 0 {
		fmt.Fprintf(&rpr, `<w:sz w:val="%d"/><w:szCs w:val="%d"/>`, sizeHalfPt, sizeHalfPt)
	}
	parts := strings.Split(text, "\n")
	var b strings.Builder
	b.WriteString(`<w:r><w:rPr>` + rpr.String() + `</w:rPr>`)
	for i, p := range parts {
		if i > 0 {
			b.WriteString(`<w:br/>`)
		}
		b.WriteString(`<w:t xml:space="preserve">` + esc(p) + `</w:t>`)
	}
	b.WriteString(`</w:r>`)
	return b.String()
}

// Heading — level 1~3 (Heading1~3 스타일). 목차 생성/문서 구조 탐색에 쓰인다.
func (d *Document) Heading(level int, text string) {
	if level < 1 {
		level = 1
	}
	if level > 3 {
		level = 3
	}
	d.blocks = append(d.blocks, fmt.Sprintf(`<w:p><w:pPr><w:pStyle w:val="Heading%d"/></w:pPr>%s</w:p>`, level, run(text, false, 0, "")))
}

// Title — 표지 제목(가운데 정렬, 크게).
func (d *Document) Title(text string) {
	d.blocks = append(d.blocks, `<w:p><w:pPr><w:pStyle w:val="Title"/><w:jc w:val="center"/></w:pPr>`+run(text, true, 40, "")+`</w:p>`)
}

// Centered — 가운데 정렬 일반 문단(표지 보조행).
func (d *Document) Centered(text string) {
	d.blocks = append(d.blocks, `<w:p><w:pPr><w:jc w:val="center"/></w:pPr>`+run(text, false, 24, "")+`</w:p>`)
}

// Paragraph — 본문 문단.
func (d *Document) Paragraph(text string) {
	d.blocks = append(d.blocks, `<w:p><w:pPr><w:pStyle w:val="Normal"/></w:pPr>`+run(text, false, 0, "")+`</w:p>`)
}

// Note — 작은 회색 안내문(문서 내 안내/확인 필요 강조 등).
func (d *Document) Note(text string) {
	d.blocks = append(d.blocks, `<w:p><w:pPr><w:pStyle w:val="Normal"/></w:pPr>`+run(text, false, 18, "666666")+`</w:p>`)
}

// Bullets — 글머리 기호 목록(numbering.xml의 numId=1).
func (d *Document) Bullets(items []string) {
	for _, it := range items {
		d.blocks = append(d.blocks, `<w:p><w:pPr><w:pStyle w:val="ListParagraph"/><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr>`+run(it, false, 0, "")+`</w:p>`)
	}
}

// Table — 첫 행을 머리글로 하는 단순 표. 열 폭은 균등(A4 본문폭 9,000 twip 기준).
func (d *Document) Table(header []string, rows [][]string) {
	cols := len(header)
	if cols == 0 {
		return
	}
	width := 9000 / cols
	var b strings.Builder
	b.WriteString(`<w:tbl><w:tblPr><w:tblStyle w:val="TableGrid"/><w:tblW w:w="9000" w:type="dxa"/>` +
		`<w:tblBorders><w:top w:val="single" w:sz="4" w:color="999999"/><w:left w:val="single" w:sz="4" w:color="999999"/><w:bottom w:val="single" w:sz="4" w:color="999999"/><w:right w:val="single" w:sz="4" w:color="999999"/><w:insideH w:val="single" w:sz="4" w:color="BBBBBB"/><w:insideV w:val="single" w:sz="4" w:color="BBBBBB"/></w:tblBorders>` +
		`<w:tblLook w:val="04A0"/></w:tblPr><w:tblGrid>`)
	for i := 0; i < cols; i++ {
		fmt.Fprintf(&b, `<w:gridCol w:w="%d"/>`, width)
	}
	b.WriteString(`</w:tblGrid>`)
	writeRow := func(cells []string, head bool) {
		b.WriteString(`<w:tr>`)
		if head {
			b.WriteString(`<w:trPr><w:tblHeader/></w:trPr>`)
		}
		for i := 0; i < cols; i++ {
			text := ""
			if i < len(cells) {
				text = cells[i]
			}
			fmt.Fprintf(&b, `<w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/>`, width)
			if head {
				b.WriteString(`<w:shd w:val="clear" w:color="auto" w:fill="EFEFEF"/>`)
			}
			b.WriteString(`</w:tcPr><w:p>` + run(text, head, 18, "") + `</w:p></w:tc>`)
		}
		b.WriteString(`</w:tr>`)
	}
	writeRow(header, true)
	for _, r := range rows {
		writeRow(r, false)
	}
	b.WriteString(`</w:tbl><w:p/>`)
	d.blocks = append(d.blocks, b.String())
}

// PageBreak — 페이지 나누기.
func (d *Document) PageBreak() {
	d.blocks = append(d.blocks, `<w:p><w:r><w:br w:type="page"/></w:r></w:p>`)
}

// TOCField — Word가 열 때/필드 갱신 시 채우는 목차 필드(제목1~3). 안내문을 결과로
// 미리 넣어 두어 갱신 전에도 빈 자리가 아니게 한다.
func (d *Document) TOCField(placeholder string) {
	d.blocks = append(d.blocks,
		`<w:p><w:r><w:fldChar w:fldCharType="begin"/></w:r>`+
			`<w:r><w:instrText xml:space="preserve"> TOC \o "1-3" \h \z \u </w:instrText></w:r>`+
			`<w:r><w:fldChar w:fldCharType="separate"/></w:r>`+
			run(placeholder, false, 18, "666666")+
			`<w:r><w:fldChar w:fldCharType="end"/></w:r></w:p>`)
}

const wNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

func (d *Document) documentXML() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<w:document ` + wNS + `><w:body>`)
	for _, blk := range d.blocks {
		b.WriteString(blk)
	}
	// A4 세로, 여백 2.5cm(1417 twip), 바닥글 참조.
	b.WriteString(`<w:sectPr><w:footerReference w:type="default" r:id="rIdFooter1"/>` +
		`<w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1417" w:right="1417" w:bottom="1417" w:left="1417" w:header="708" w:footer="708" w:gutter="0"/></w:sectPr>`)
	b.WriteString(`</w:body></w:document>`)
	return b.String()
}

func (d *Document) footerXML() string {
	note := ""
	if d.FooterNote != "" {
		note = run(d.FooterNote+"    ", false, 16, "888888")
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:ftr ` + wNS + `><w:p><w:pPr><w:jc w:val="right"/></w:pPr>` + note +
		`<w:r><w:rPr><w:sz w:val="18"/></w:rPr><w:fldChar w:fldCharType="begin"/></w:r>` +
		`<w:r><w:rPr><w:sz w:val="18"/></w:rPr><w:instrText xml:space="preserve"> PAGE </w:instrText></w:r>` +
		`<w:r><w:rPr><w:sz w:val="18"/></w:rPr><w:fldChar w:fldCharType="separate"/></w:r>` +
		`<w:r><w:rPr><w:sz w:val="18"/></w:rPr><w:t>1</w:t></w:r>` +
		`<w:r><w:rPr><w:sz w:val="18"/></w:rPr><w:fldChar w:fldCharType="end"/></w:r></w:p></w:ftr>`
}

const stylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Malgun Gothic" w:hAnsi="Malgun Gothic" w:eastAsia="맑은 고딕" w:cs="Malgun Gothic"/><w:sz w:val="20"/><w:szCs w:val="20"/><w:lang w:val="ko-KR" w:eastAsia="ko-KR"/></w:rPr></w:rPrDefault><w:pPrDefault><w:pPr><w:spacing w:after="120" w:line="300" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults>
<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/><w:qFormat/></w:style>
<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/><w:basedOn w:val="Normal"/><w:qFormat/><w:pPr><w:spacing w:before="2400" w:after="480"/><w:jc w:val="center"/></w:pPr><w:rPr><w:b/><w:sz w:val="40"/><w:szCs w:val="40"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="480" w:after="200"/><w:outlineLvl w:val="0"/></w:pPr><w:rPr><w:b/><w:sz w:val="32"/><w:szCs w:val="32"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="320" w:after="120"/><w:outlineLvl w:val="1"/></w:pPr><w:rPr><w:b/><w:sz w:val="26"/><w:szCs w:val="26"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="heading 3"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/><w:pPr><w:keepNext/><w:spacing w:before="200" w:after="80"/><w:outlineLvl w:val="2"/></w:pPr><w:rPr><w:b/><w:sz w:val="22"/><w:szCs w:val="22"/></w:rPr></w:style>
<w:style w:type="paragraph" w:styleId="ListParagraph"><w:name w:val="List Paragraph"/><w:basedOn w:val="Normal"/><w:qFormat/><w:pPr><w:ind w:left="720"/><w:contextualSpacing/></w:pPr></w:style>
<w:style w:type="table" w:styleId="TableGrid"><w:name w:val="Table Grid"/><w:tblPr><w:tblCellMar><w:top w:w="60" w:type="dxa"/><w:left w:w="100" w:type="dxa"/><w:bottom w:w="60" w:type="dxa"/><w:right w:w="100" w:type="dxa"/></w:tblCellMar></w:tblPr></w:style>
</w:styles>`

const numberingXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:numbering xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:abstractNum w:abstractNumId="0"><w:multiLevelType w:val="hybridMultilevel"/>
<w:lvl w:ilvl="0"><w:start w:val="1"/><w:numFmt w:val="bullet"/><w:lvlText w:val="•"/><w:lvlJc w:val="left"/><w:pPr><w:ind w:left="720" w:hanging="360"/></w:pPr></w:lvl>
</w:abstractNum>
<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num>
</w:numbering>`

const contentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>
<Override PartName="/word/numbering.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/>
<Override PartName="/word/footer1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml"/>
<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
<Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>
</Types>`

const rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/>
</Relationships>`

const documentRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rIdStyles" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
<Relationship Id="rIdNumbering" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering" Target="numbering.xml"/>
<Relationship Id="rIdFooter1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"/>
</Relationships>`

const appXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes"><Application>biz-platform</Application></Properties>`

func (d *Document) coreXML() string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:dcmitype="http://purl.org/dc/dcmitype/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">` +
		`<dc:title>` + esc(d.CoreTitle) + `</dc:title><dc:creator>` + esc(d.CoreCreator) + `</dc:creator></cp:coreProperties>`
}

// WriteTo — 완성된 .docx(zip)를 w에 쓴다.
func (d *Document) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	parts := []struct{ name, body string }{
		{"[Content_Types].xml", contentTypesXML},
		{"_rels/.rels", rootRelsXML},
		{"docProps/core.xml", d.coreXML()},
		{"docProps/app.xml", appXML},
		{"word/document.xml", d.documentXML()},
		{"word/_rels/document.xml.rels", documentRelsXML},
		{"word/styles.xml", stylesXML},
		{"word/numbering.xml", numberingXML},
		{"word/footer1.xml", d.footerXML()},
	}
	for _, p := range parts {
		f, err := zw.Create(p.name)
		if err != nil {
			return 0, err
		}
		if _, err := f.Write([]byte(p.body)); err != nil {
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

// Bytes — 편의 함수.
func (d *Document) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	if _, err := d.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
