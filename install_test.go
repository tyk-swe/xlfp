package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInstallScript runs install.sh against a local fake release server:
// the happy path must install the (fake) binary, and a tampered checksum
// must fail closed with nothing installed.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh covers linux/darwin; windows uses install.ps1")
	}
	for _, tool := range []string{"sh", "curl", "tar", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	asset := fmt.Sprintf("xlfp_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	// Fabricate a release archive holding a fake xlfp binary.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "#!/bin/sh\necho 'xlfp test-double'\n"
	if err := tw.WriteHeader(&tar.Header{Name: "xlfp", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	goodChecksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)

	serve := func(checksums string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/" + asset:
				_, _ = w.Write(archive)
			case "/checksums.txt":
				_, _ = fmt.Fprint(w, checksums)
			default:
				http.NotFound(w, r)
			}
		}))
	}
	runInstaller := func(t *testing.T, baseURL, installDir string) (string, error) {
		t.Helper()
		cmd := exec.Command("sh", "install.sh")
		cmd.Env = append(os.Environ(),
			"XLFP_RELEASE_BASE_URL="+baseURL,
			"XLFP_INSTALL_DIR="+installDir,
			"XLFP_VERSION=",
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("happy path installs", func(t *testing.T) {
		srv := serve(goodChecksums)
		defer srv.Close()
		installDir := filepath.Join(t.TempDir(), "bin")
		out, err := runInstaller(t, srv.URL, installDir)
		if err != nil {
			t.Fatalf("install.sh failed: %v\n%s", err, out)
		}
		bin := filepath.Join(installDir, "xlfp")
		got, err := exec.Command(bin).CombinedOutput()
		if err != nil || !strings.Contains(string(got), "test-double") {
			t.Fatalf("installed binary: out %q err %v (installer said:\n%s)", got, err, out)
		}
	})

	t.Run("directory destination fails closed", func(t *testing.T) {
		srv := serve(goodChecksums)
		defer srv.Close()
		installDir := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(filepath.Join(installDir, "xlfp"), 0o700); err != nil {
			t.Fatal(err)
		}
		out, err := runInstaller(t, srv.URL, installDir)
		if err == nil {
			t.Fatalf("installer reported success for a directory destination: %s", out)
		}
		if _, err := os.Stat(filepath.Join(installDir, "xlfp", "xlfp")); !os.IsNotExist(err) {
			t.Fatal("binary incorrectly installed inside destination directory")
		}
	})

	t.Run("tampered checksum fails closed", func(t *testing.T) {
		bad := []byte(goodChecksums)
		if bad[0] == '0' {
			bad[0] = '1'
		} else {
			bad[0] = '0'
		}
		srv := serve(string(bad))
		defer srv.Close()
		installDir := filepath.Join(t.TempDir(), "bin")
		out, err := runInstaller(t, srv.URL, installDir)
		if err == nil {
			t.Fatalf("install.sh succeeded with a bad checksum:\n%s", out)
		}
		if !strings.Contains(out, "checksum mismatch") {
			t.Fatalf("expected checksum mismatch message, got:\n%s", out)
		}
		if _, statErr := os.Stat(filepath.Join(installDir, "xlfp")); !os.IsNotExist(statErr) {
			t.Fatal("binary was installed despite checksum mismatch")
		}
	})
}
