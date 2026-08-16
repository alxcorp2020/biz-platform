"""worker.py 단위 테스트(네트워크/DB 없이 순수 함수 + 다운로드 검증 경로).

실행: cd analyzer && ./venv/bin/python -m unittest discover -s tests -v
통합(CASE A~H)은 로컬 Postgres + 테스트 HTTP 서버가 필요해 이 파일에 넣지 않았다
(docs/HANDOFF.md 절차 참고).
"""
import ipaddress
import os
import sys
import time
import unittest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
import worker  # noqa: E402


class _Cfg:
    allow_private_urls = False
    download_timeout = 5
    max_download_bytes = 1024


class HostAllowTest(unittest.TestCase):
    def test_gov_domains_allowed(self):
        for h in ("www.g2b.go.kr", "www.bizinfo.go.kr", "keris.or.kr.go.kr", "go.kr"):
            self.assertTrue(worker.host_allowed(h), h)

    def test_other_domains_rejected(self):
        for h in ("", "example.com", "g2b.go.kr.evil.com", "localhost", "127.0.0.1", "gokr"):
            self.assertFalse(worker.host_allowed(h), h)


class BlockedIPTest(unittest.TestCase):
    def test_blocked(self):
        for ip in ("127.0.0.1", "10.0.0.1", "172.16.5.5", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fe80::1"):
            self.assertTrue(worker.is_blocked_ip(ipaddress.ip_address(ip)), ip)

    def test_public_ok(self):
        for ip in ("8.8.8.8", "211.32.10.10", "2001:4860:4860::8888"):
            self.assertFalse(worker.is_blocked_ip(ipaddress.ip_address(ip)), ip)


class ValidateURLTest(unittest.TestCase):
    def test_scheme_and_host(self):
        cfg = _Cfg()
        with self.assertRaises(worker.DownloadError):
            worker.validate_url("file:///etc/passwd", cfg)
        with self.assertRaises(worker.DownloadError):
            worker.validate_url("ftp://www.g2b.go.kr/x", cfg)
        with self.assertRaises(worker.DownloadError):
            worker.validate_url("http://example.com/a.pdf", cfg)
        with self.assertRaises(worker.DownloadError):
            worker.validate_url("http://127.0.0.1:8891/a.pdf", cfg)  # allowlist 실패

    def test_private_bypass_only_when_flag(self):
        cfg = _Cfg()
        cfg.allow_private_urls = True
        u = worker.validate_url("http://127.0.0.1:8891/a.pdf", cfg)
        self.assertEqual(u.hostname, "127.0.0.1")

    def test_download_error_transient_flag(self):
        self.assertTrue(worker.DownloadError("x", transient=True).transient)
        self.assertFalse(worker.DownloadError("x").transient)


class MagicTest(unittest.TestCase):
    def test_kinds(self):
        self.assertEqual(worker.magic_kind(b"%PDF-1.4 ..."), "pdf")
        self.assertEqual(worker.magic_kind(b"PK\x03\x04rest"), "zip")
        self.assertEqual(worker.magic_kind(b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1rest"), "ole")
        self.assertIsNone(worker.magic_kind(b"hello"))
        self.assertIsNone(worker.magic_kind(b""))

    def test_expected_by_ext(self):
        self.assertEqual(worker.EXPECTED_MAGIC["pdf"], "pdf")
        self.assertEqual(worker.EXPECTED_MAGIC["hwpx"], "zip")
        self.assertEqual(worker.EXPECTED_MAGIC["hwp"], "ole")
        self.assertEqual(worker.EXPECTED_MAGIC["xlsx"], "zip")
        self.assertNotIn("docx", worker.EXPECTED_MAGIC)  # 미지원 → unsupported 경로

    def test_normalized_type(self):
        self.assertEqual(worker.normalized_type("PDF", "a.pdf"), "pdf")
        self.assertEqual(worker.normalized_type("", "공고서.HWPX"), "hwpx")
        self.assertEqual(worker.normalized_type(None, "noext"), "")
        self.assertEqual(worker.normalized_type(".hwp", None), "hwp")


class RedactTest(unittest.TestCase):
    def test_query_removed(self):
        self.assertEqual(
            worker._redacted("https://www.g2b.go.kr/pn/x/downloadFile.do?bidPbancNo=R1&token=SECRET"),
            "https://www.g2b.go.kr/pn/x/downloadFile.do",
        )


class ParseTimeoutTest(unittest.TestCase):
    def test_parse_timeout_raises_extraction_error(self):
        orig = worker.EXTRACTORS.get("pdf")
        worker.EXTRACTORS["pdf"] = lambda path: time.sleep(5) or "never"
        try:
            with self.assertRaises(worker.ExtractionError):
                worker.extract_bytes(b"%PDF-fake", "pdf", parse_timeout=1)
        finally:
            worker.EXTRACTORS["pdf"] = orig

    def test_temp_file_removed(self):
        seen = {}
        orig = worker.EXTRACTORS.get("pdf")
        worker.EXTRACTORS["pdf"] = lambda path: seen.setdefault("path", path) and "text\x00with nul"
        try:
            text = worker.extract_bytes(b"%PDF-fake", "pdf")
            self.assertEqual(text, "textwith nul")
            self.assertFalse(os.path.exists(seen["path"]))
        finally:
            worker.EXTRACTORS["pdf"] = orig


class ScopeTest(unittest.TestCase):
    def test_build_scope_modes(self):
        from datetime import datetime, timezone

        class A:
            attachment_id = []
            notice_id = None
            limit = None
            created_after = None
            once = False

        cfg = worker.Config()
        cfg.process_existing = False
        cfg.created_after = None
        started = datetime(2026, 8, 16, tzinfo=timezone.utc)
        scope, once, mode = worker.build_scope(A(), cfg, started)
        self.assertEqual(mode, "new-only")
        self.assertEqual(scope.created_after, started.isoformat())
        self.assertFalse(once)

        cfg.process_existing = True
        scope, once, mode = worker.build_scope(A(), cfg, started)
        self.assertEqual(mode, "all-pending")
        self.assertIsNone(scope.created_after)

        a = A()
        a.notice_id = "00000000-0000-0000-0000-000000000000"
        scope, once, mode = worker.build_scope(a, cfg, started)
        self.assertEqual(mode, "backfill")
        self.assertTrue(once)


if __name__ == "__main__":
    unittest.main()
