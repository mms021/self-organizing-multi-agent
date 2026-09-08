package artifactpolicy

import "testing"

func TestArtifactURI(t *testing.T) {
	for _, raw := range []string{"https://example.com/a.EXE", "https://example.com/report.exe.txt", "https://example.com/a%2Eexe", "https://example.com/a%252eexe", "https://example.com/a.sh", "https://example.com/a.zip", "https://example.com/a.svg", "https://example.com/download?filename=evil.ps1", "file:///tmp/log.txt", "https://example.com/a.txt:evil.exe", "https://example.com/a.exe%20", "https://example.com/a\u202etxt.exe"} {
		if URI(raw) == nil {
			t.Errorf("allowed %q", raw)
		}
	}
	for _, raw := range []string{"https://example.com/report.txt", "https://example.com/report.v1.json", "https://example.com/download?token=opaque", "https://example.com/test.log"} {
		if err := URI(raw); err != nil {
			t.Errorf("rejected %q: %v", raw, err)
		}
	}
}

func TestContentCannotMasqueradeAsText(t *testing.T) {
	for _, body := range []string{"MZpretend text", "\x7fELF", "\x00asm", "PK\x03\x04archive", "\x1f\x8bcompressed", "#!/bin/sh\necho bad", "\ufeff#!/usr/bin/python\nprint(1)", "@echo off\necho bad", "<html>page</html>", "<svg onload='bad'>", "<!DOCTYPE x [<!ENTITY e SYSTEM 'file:///etc/passwd'>]>", "<script>bad()</script>", "a\x00b", "a\x1bb"} {
		if Content([]byte(body), "text/plain") == nil {
			t.Errorf("accepted %q", body)
		}
	}
	for _, body := range []string{"3 tests passed\n", "Ожидание: 4\nПолучено: 4", "{\"passed\":3}", "<tests passed=\"3\"/>"} {
		if err := Content([]byte(body), "text/plain"); err != nil {
			t.Errorf("rejected %q: %v", body, err)
		}
	}
	if Content([]byte("not json"), "application/json") == nil {
		t.Fatal("invalid JSON accepted")
	}
	for _, media := range []string{"text/html", "text/javascript", "application/javascript", "application/zip", "image/svg+xml"} {
		if MediaType(media) {
			t.Errorf("allowed active media %s", media)
		}
	}
}
