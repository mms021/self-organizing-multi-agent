package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"aichatdeck/internal/model"
)

type artifactRoundTrip func(*http.Request) (*http.Response, error)

func (f artifactRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func contentHash(body string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))) }

func TestFetchArtifactContent(t *testing.T) {
	for _, scenario := range []string{"valid", "checksum", "oversized", "long-text", "binary", "redirect", "gzip", "media", "http", "private", "credentials", "port", "bad-hash", "read-limit", "cancelled", "disposition", "encoded-disposition", "fake-text", "script", "html"} {
		t.Run(scenario, func(t *testing.T) {
			body := "tests: 3 passed\n"
			artifact := model.Artifact{ArtifactID: "artifact", URI: "https://example.com/result", Checksum: contentHash(body)}
			status, media, encoding := 200, "text/plain; charset=utf-8", ""
			length := int64(len(body))
			disposition := ""
			switch scenario {
			case "disposition":
				disposition = `attachment; filename="report.exe.txt"`
			case "encoded-disposition":
				disposition = `attachment; filename*=UTF-8''run%2Esh`
			case "fake-text":
				body = "MZfake executable"
				artifact.Checksum = contentHash(body)
			case "script":
				body = "#!/bin/sh\necho bad"
				artifact.Checksum = contentHash(body)
			case "html":
				body = "<html><script>bad()</script></html>"
				artifact.Checksum = contentHash(body)
			case "checksum":
				artifact.Checksum = contentHash("different")
			case "oversized":
				length = maxArtifactBytes + 1
			case "long-text":
				body = strings.Repeat("a", maxArtifactRunes+1)
				artifact.Checksum = contentHash(body)
				length = int64(len(body))
			case "binary":
				body = "\x00\xff"
				artifact.Checksum = contentHash(body)
			case "redirect":
				status = 302
			case "gzip":
				encoding = "gzip"
			case "media":
				media = "application/octet-stream"
			case "http":
				artifact.URI = "http://example.com/result"
			case "private":
				artifact.URI = "https://127.0.0.1/result"
			case "credentials":
				artifact.URI = "https://user:password@example.com/result"
			case "port":
				artifact.URI = "https://example.com:8443/result"
			case "bad-hash":
				artifact.Checksum = "not-a-hash"
			case "read-limit":
				body = strings.Repeat("a", maxArtifactBytes+1)
				length = -1
			}
			requests := 0
			client := artifactHTTPClient()
			client.Transport = artifactRoundTrip(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Accept-Encoding") != "identity" {
					t.Fatal("unsafe artifact request headers")
				}
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Disposition": {disposition}, "Content-Type": {media}, "Content-Encoding": {encoding}, "Location": {"https://127.0.0.1/secret"}}, ContentLength: length, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			got, err := fetchArtifactContent(ctx, client, artifact)
			if scenario == "valid" {
				if err != nil || got.Text != body || got.ArtifactID != artifact.ArtifactID || got.SHA256 != strings.TrimPrefix(artifact.Checksum, "sha256:") {
					t.Fatalf("content=%+v err=%v", got, err)
				}
			} else if err == nil || got.Text != "" {
				t.Fatalf("unsafe content accepted: %+v", got)
			}
			if requests > 1 {
				t.Fatal("redirect followed")
			}
			if (scenario == "http" || scenario == "private" || scenario == "credentials" || scenario == "port" || scenario == "bad-hash") && requests != 0 {
				t.Fatal("invalid artifact reached transport")
			}
		})
	}
}

func TestArtifactAddressPolicy(t *testing.T) {
	for _, address := range []string{"0.0.0.0", "10.0.0.1", "100.100.100.200", "127.0.0.1", "169.254.169.254", "172.16.0.1", "192.168.1.1", "192.0.2.1", "198.19.1.1", "224.0.0.1", "168.63.129.16", "::", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "64:ff9b::7f00:1", "2002:7f00:1::", "2001:db8::1"} {
		if publicArtifactIP(netip.MustParseAddr(address)) {
			t.Errorf("allowed %s", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicArtifactIP(netip.MustParseAddr(address)) {
			t.Errorf("blocked public address %s", address)
		}
	}
}

func TestArtifactDNSIsPinnedAndMixedAnswersRejected(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		resolves, dials := 0, 0
		dial := artifactDial(func(context.Context, string, string) ([]netip.Addr, error) {
			resolves++
			ips := []netip.Addr{netip.MustParseAddr("8.8.8.8")}
			if mixed {
				ips = append(ips, netip.MustParseAddr("127.0.0.1"))
			}
			return ips, nil
		}, func(_ context.Context, network, address string) (net.Conn, error) {
			dials++
			if network != "tcp" || address != "8.8.8.8:443" {
				t.Fatalf("hostname re-resolved: %s %s", network, address)
			}
			return nil, nil
		})
		_, err := dial(context.Background(), "tcp", "example.com:443")
		if resolves != 1 || (mixed && (err == nil || dials != 0)) || (!mixed && (err != nil || dials != 1)) {
			t.Fatalf("mixed=%v resolves=%d dials=%d err=%v", mixed, resolves, dials, err)
		}
	}
}

func TestArtifactTransportDoesNotInheritProxy(t *testing.T) {
	client := artifactHTTPClient()
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil || client.Jar != nil || client.Timeout == 0 || !transport.DisableCompression {
		t.Fatal("unsafe transport defaults")
	}
}
