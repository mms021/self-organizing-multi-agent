package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"aichatdeck/internal/artifactpolicy"
	"aichatdeck/internal/model"
)

const maxArtifactBytes = 64 << 10
const maxArtifactRunes = 3500 // fit a complete content block into the prompt
const maxVerificationArtifacts = 8

type ArtifactContent struct {
	ArtifactID string `json:"artifact_id"`
	Text       string `json:"text"`
	SHA256     string `json:"sha256"`
}

// The transport is independent of the platform client: no platform credentials,
// ambient proxy, cookies, redirects or automatic decompression are inherited.
func artifactHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext:            artifactDial(net.DefaultResolver.LookupNetIP, (&net.Dialer{Timeout: 3 * time.Second}).DialContext),
			DisableCompression:     true,
			DisableKeepAlives:      true,
			TLSHandshakeTimeout:    3 * time.Second,
			ResponseHeaderTimeout:  3 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
		},
	}
}

// Resolve once and dial the validated IP, not the hostname, to prevent DNS
// rebinding. TLS still validates the certificate against the original hostname.
func artifactDial(resolve func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, errors.New("artifact port is not allowed")
		}
		ips, err := resolve(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("artifact DNS resolution failed")
		}
		for _, ip := range ips {
			if !publicArtifactIP(ip) {
				return nil, errors.New("artifact address is not public")
			}
		}
		return dial(ctx, "tcp", net.JoinHostPort(ips[0].String(), port))
	}
}

var artifactBlockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/3"),
	netip.MustParsePrefix("168.63.129.16/32"), // Azure host virtual service address
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicArtifactIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	// Conservatively reject IPv6 outside native global-unicast allocation,
	// including NAT64 and other transition mechanisms.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range artifactBlockedNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func fetchArtifactContent(ctx context.Context, client *http.Client, artifact model.Artifact) (ArtifactContent, error) {
	fail := func(reason string) (ArtifactContent, error) { return ArtifactContent{}, errors.New(reason) }
	if err := artifactpolicy.URI(artifact.URI); err != nil {
		return fail(err.Error())
	}
	want, err := hex.DecodeString(strings.TrimPrefix(artifact.Checksum, "sha256:"))
	if err != nil || len(want) != sha256.Size {
		return fail("artifact requires a SHA-256 checksum")
	}
	u, err := url.Parse(artifact.URI)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return fail("artifact requires an HTTPS URL on port 443 without credentials or fragment")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicArtifactIP(ip) {
		return fail("artifact address is not public")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fail("invalid artifact request")
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return fail("artifact download failed")
	} // do not expose signed URL query strings
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail("artifact did not return HTTP 200")
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return fail("encoded artifacts are not supported")
	}
	if resp.ContentLength > maxArtifactBytes {
		return fail("artifact exceeds size limit")
	}
	media, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !artifactpolicy.MediaType(media) {
		return fail("artifact must be text, JSON or XML")
	}
	if disposition := resp.Header.Get("Content-Disposition"); disposition != "" {
		_, values, err := mime.ParseMediaType(disposition)
		if err != nil {
			return fail("invalid artifact Content-Disposition")
		}
		if err := artifactpolicy.Filename(values["filename"]); err != nil {
			return fail(err.Error())
		}
	}
	if charset := strings.ToLower(params["charset"]); charset != "" && charset != "utf-8" && charset != "us-ascii" {
		return fail("artifact encoding must be UTF-8")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err != nil || len(body) > maxArtifactBytes {
		return fail("artifact read failed or exceeds size limit")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(want) {
		return fail("artifact SHA-256 mismatch")
	}
	if !utf8.Valid(body) || strings.ContainsRune(string(body), 0) || utf8.RuneCount(body) > maxArtifactRunes {
		return fail("artifact is not bounded UTF-8 text")
	}
	if err := artifactpolicy.Content(body, media); err != nil {
		return fail(err.Error())
	}
	return ArtifactContent{ArtifactID: artifact.ArtifactID, Text: string(body), SHA256: hex.EncodeToString(sum[:])}, nil
}
