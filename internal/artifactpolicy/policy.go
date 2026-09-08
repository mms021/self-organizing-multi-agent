// Package artifactpolicy defines the narrow, non-executable evidence formats.
// This is format validation, not malware detection or permission to execute data.
package artifactpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var dangerousSuffix = regexp.MustCompile(`(?i)\.(exe|dll|com|scr|msi|msp|bat|cmd|ps1|psm1|vbs|vbe|js|mjs|cjs|jse|wsf|wsh|hta|sh|bash|zsh|py|pyc|pl|rb|php|jar|class|wasm|so|dylib|app|dmg|iso|zip|gz|tgz|tar|7z|rar|docm|xlsm|pptm|lnk|desktop|html?|svg)(\.|$)`)
var activeMarkup = regexp.MustCompile(`(?i)<\s*(script|iframe|object|embed|svg|html|!doctype|!entity)\b`)

func Filename(name string) error {
	if strings.ContainsAny(name, "\\:\x00") || strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return errors.New("unsafe artifact filename")
	}
	name = strings.TrimSpace(name)
	if strings.HasSuffix(name, ".") || dangerousSuffix.MatchString(name) {
		return errors.New("executable, active document or archive filename is not allowed")
	}
	switch strings.ToLower(path.Ext(name)) {
	case "", ".txt", ".log", ".json", ".xml", ".csv", ".tsv", ".md", ".ndjson":
		return nil
	default:
		return errors.New("artifact filename must identify a supported text format")
	}
}

func URI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return errors.New("artifact URI must be HTTPS on port 443 without credentials or fragment")
	}
	// Path is already percent-decoded by url.Parse. Reject nested encodings
	// rather than interpreting a filename differently from the remote server.
	if strings.Contains(u.Path, "%") {
		return errors.New("nested filename encoding is not allowed")
	}
	if err := Filename(path.Base(u.Path)); err != nil {
		return err
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("invalid artifact URI query")
	}
	for key, values := range query {
		if strings.EqualFold(key, "filename") || strings.EqualFold(key, "file") || strings.EqualFold(key, "name") {
			for _, value := range values {
				if err := Filename(value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func MediaType(media string) bool {
	switch media {
	case "text/plain", "text/csv", "text/tab-separated-values", "text/markdown", "text/xml", "application/json", "application/xml", "application/x-ndjson":
		return true
	}
	return false
}

func Content(body []byte, media string) error {
	for _, signature := range [][]byte{[]byte("MZ"), []byte("\x7fELF"), []byte("\x00asm"), {0xfe, 0xed, 0xfa, 0xce}, {0xce, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xcf}, {0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}, []byte("PK\x03\x04"), []byte("PK\x05\x06"), []byte("Rar!"), {0x1f, 0x8b}, {0x37, 0x7a, 0xbc, 0xaf, 0x27, 0x1c}, {0xd0, 0xcf, 0x11, 0xe0}, []byte("%PDF-")} {
		if bytes.HasPrefix(body, signature) {
			return errors.New("binary, executable or container signature is not allowed")
		}
	}
	if len(body) > 262 && string(body[257:262]) == "ustar" {
		return errors.New("archive is not allowed")
	}
	if !utf8.Valid(body) {
		return errors.New("artifact must be UTF-8 text")
	}
	for _, r := range string(body) {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return errors.New("binary control bytes are not allowed")
		}
	}
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(body)), "\ufeff"))
	lower := strings.ToLower(text)
	if strings.HasPrefix(text, "#!") || strings.HasPrefix(lower, "@echo off") || strings.HasPrefix(lower, "#requires ") || activeMarkup.MatchString(text) {
		return errors.New("script or active markup is not allowed")
	}
	if media == "application/json" && !json.Valid(body) {
		return errors.New("invalid JSON artifact")
	}
	return nil
}
