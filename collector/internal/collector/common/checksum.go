package common

import (
	"crypto/sha256"
	"encoding/hex"
)

// Sha256Hex hashes raw content for storage in raw_documents.content_hash and
// attachments.file_hash (used for change detection and dedup, spec 6.5/6.6).
func Sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// DedupKey builds the composite identity used when a source has no reliable
// notice number (spec 6.5: "공고번호가 없는 출처는 복합 식별키를 사용한다").
func DedupKey(orgName, title, publishedAt string) string {
	return Sha256Hex([]byte(orgName + "|" + title + "|" + publishedAt))
}
