package worker

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateNDSRemoteURLGuard(t *testing.T) {
	oldLookup := ndsLookupIP
	defer func() { ndsLookupIP = oldLookup }()
	ndsLookupIP = func(_ context.Context, _ string, host string) ([]net.IP, error) {
		switch host {
		case "cdn.rebitplay.com", "roms.b-cdn.net":
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		case "private.rebitplay.com":
			return []net.IP{net.ParseIP("10.0.0.2")}, nil
		default:
			return nil, fmt.Errorf("unexpected host %s", host)
		}
	}
	t.Setenv("NDS_DOWNLOAD_ALLOWED_HOSTS", "*.b-cdn.net,cdn.rebitplay.com,private.rebitplay.com")

	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{name: "allowlisted exact host", rawURL: "https://cdn.rebitplay.com/rom.nds"},
		{name: "allowlisted wildcard host", rawURL: "https://roms.b-cdn.net/rom.nds"},
		{name: "private ip rejected", rawURL: "https://private.rebitplay.com/rom.nds", wantErr: true},
		{name: "disallowed host rejected", rawURL: "https://evil.example/rom.nds", wantErr: true},
		{name: "unsupported scheme rejected", rawURL: "file:///tmp/rom.nds", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNDSRemoteURL(tt.rawURL)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRequestDownloadRejectsRedirectToPrivate(t *testing.T) {
	oldLookup := ndsLookupIP
	defer func() { ndsLookupIP = oldLookup }()
	ndsLookupIP = func(_ context.Context, _ string, host string) ([]net.IP, error) {
		if host == "10.0.0.1" {
			return []net.IP{net.ParseIP("10.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	t.Setenv("NDS_DOWNLOAD_ALLOWED_HOSTS", "127.0.0.1,10.0.0.1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/private.nds", http.StatusFound)
	}))
	defer server.Close()

	if _, err := requestDownload(server.URL, maxNDSRomBytes); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("redirect-to-private error = %v", err)
	}
}

func TestRequestDownloadRejectsOversizeContentLength(t *testing.T) {
	withPublicLocalhost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", maxNDSSaveBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resp, err := requestDownload(server.URL, maxNDSSaveBytes)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "content-length") {
		t.Fatalf("oversize content-length error = %v", err)
	}
}

func TestInstallNDSROMRejectsSHA1Mismatch(t *testing.T) {
	withPublicLocalhost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not the expected rom"))
	}))
	defer server.Close()

	err := installNDSROM(server.URL+"/rom.nds", filepath.Join(t.TempDir(), "nds", strings.Repeat("0", 40)+".nds"), "rom.nds", strings.Repeat("0", 40))
	if err == nil || !strings.Contains(err.Error(), "SHA1 mismatch") {
		t.Fatalf("sha1 mismatch error = %v", err)
	}
}

func TestInstallNDSROMContentAddressedCache(t *testing.T) {
	withPublicLocalhost(t)
	rom := []byte("expected rom bytes")
	sum := sha1.Sum(rom)
	sha := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(rom)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "nds", sha+".nds")
	if err := installNDSROM(server.URL+"/rom.nds", path, "rom.nds", sha); err != nil {
		t.Fatal(err)
	}
	if !validCachedNDSROM(path, sha) {
		t.Fatal("cached ROM did not validate by SHA1")
	}
}

func withPublicLocalhost(t *testing.T) {
	t.Helper()
	oldLookup := ndsLookupIP
	t.Cleanup(func() { ndsLookupIP = oldLookup })
	ndsLookupIP = func(_ context.Context, _ string, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	t.Setenv("NDS_DOWNLOAD_ALLOWED_HOSTS", "127.0.0.1")
}
