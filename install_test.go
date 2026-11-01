package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mockInstallerDownloads(t *testing.T, payload string) string {
	t.Helper()
	script, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	curl := `#!/bin/sh
set -eu
url= output=
while [ "$#" -gt 0 ]; do
    case "$1" in
        https://*) url=$1 ;;
        -o) shift; output=$1 ;;
    esac
    shift
done
case "$url" in
    https://raw.githubusercontent.com/agent0ai/codex-bridge/main/install.sh)
        case "${TEST_INSTALL_FAILURE:-}" in
            script-download) printf 'touch "%s"\n' "$TEST_INSTALL_MARKER"; exit 22 ;;
            empty-script) exit 0 ;;
        esac
        cat "$TEST_INSTALL_SCRIPT"
        ;;
    https://github.com/agent0ai/codex-bridge/releases/latest/download/codex-bridge-*)
        printf '%s' "$url" > "$TEST_INSTALL_URL"
        case "${TEST_INSTALL_FAILURE:-}" in
            binary-download) printf partial > "$output"; exit 22 ;;
            invalid-binary) printf '#!/bin/sh\necho invalid\n' > "$output"; exit 0 ;;
        esac
        cp "$TEST_INSTALL_PAYLOAD" "$output"
        ;;
    *) echo "Unexpected URL: $url" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_INSTALL_SCRIPT", script)
	t.Setenv("TEST_INSTALL_PAYLOAD", payload)
	t.Setenv("TEST_INSTALL_URL", filepath.Join(dir, "url"))
	t.Setenv("TEST_INSTALL_MARKER", filepath.Join(dir, "should-not-execute"))
	t.Setenv("TEST_INSTALL_FAILURE", "")
	return dir
}

func TestInstaller(t *testing.T) {
	for _, tc := range []struct{ os, arch, asset, failure string }{
		{"Linux", "x86_64", "linux-amd64", ""},
		{"Linux", "aarch64", "linux-arm64", ""},
		{"Darwin", "x86_64", "darwin-amd64", ""},
		{"Darwin", "arm64", "darwin-arm64", ""},
		{"FreeBSD", "amd64", "", "unsupported"},
		{"Linux", "i686", "", "unsupported"},
		{"Linux", "amd64", "linux-amd64", "binary-download"},
		{"Linux", "arm64", "linux-arm64", "invalid-binary"},
	} {
		t.Run(tc.os+"/"+tc.arch+"/"+tc.failure, func(t *testing.T) {
			dir := t.TempDir()
			payload := filepath.Join(dir, "release")
			const binary = "#!/bin/sh\nprintf 'codex-bridge test-release\\n'\n"
			if err := os.WriteFile(payload, []byte(binary), 0700); err != nil {
				t.Fatal(err)
			}
			bin := mockInstallerDownloads(t, payload)
			uname := "#!/bin/sh\ncase \"$1\" in\n-s) echo \"$TEST_INSTALL_OS\" ;;\n-m) echo \"$TEST_INSTALL_ARCH\" ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(bin, "uname"), []byte(uname), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TEST_INSTALL_OS", tc.os)
			t.Setenv("TEST_INSTALL_ARCH", tc.arch)
			t.Setenv("TEST_INSTALL_FAILURE", tc.failure)
			destination := filepath.Join(dir, "custom binary")
			if err := os.WriteFile(destination, []byte("old binary"), 0700); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command("sh", os.Getenv("TEST_INSTALL_SCRIPT"), destination).CombinedOutput()
			got, readErr := os.ReadFile(destination)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.failure == "" {
				info, statErr := os.Stat(destination)
				if err != nil || string(got) != binary || statErr != nil || info.Mode().Perm() != 0755 {
					t.Fatal("installation failed:", err, string(output), statErr)
				}
			} else if err == nil || string(got) != "old binary" {
				t.Fatal("failed installation changed existing binary:", err, string(output))
			}
			if tc.asset != "" {
				url, err := os.ReadFile(os.Getenv("TEST_INSTALL_URL"))
				if err != nil || string(url) != "https://github.com/agent0ai/codex-bridge/releases/latest/download/codex-bridge-"+tc.asset {
					t.Fatal("wrong asset:", string(url), err)
				}
			}
			leftovers, _ := filepath.Glob(filepath.Join(dir, ".codex-bridge-install.*"))
			if len(leftovers) != 0 {
				t.Fatal("installer left temporary files:", leftovers)
			}
		})
	}
}

func TestUpdateInstallsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "release")
	if output, err := exec.Command("go", "build", "-o", payload, ".").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	binary, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "custom bridge")
	if err := os.WriteFile(destination, binary, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bridge-link")
	if err := os.Symlink(destination, link); err != nil {
		t.Fatal(err)
	}
	mockInstallerDownloads(t, payload)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	path := filepath.Join(dir, "custom config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("listen: %s\nmodels: []\n", address)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command(payload, "--config", path, "stop").Run() })
	if output, err := exec.Command(link, "--config", path, "start").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	for _, failure := range []string{"script-download", "empty-script", "binary-download", "invalid-binary", ""} {
		t.Run("failure="+failure, func(t *testing.T) {
			t.Setenv("TEST_INSTALL_FAILURE", failure)
			before, err := os.Stat(destination)
			if err != nil {
				t.Fatal(err)
			}
			output, updateErr := exec.Command(link, "update", "--config", path).CombinedOutput()
			after, err := os.Stat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "" {
				if updateErr != nil || os.SameFile(before, after) || !bytes.Contains(output, []byte("Installed")) || !bytes.Contains(output, []byte("Stopped")) || !bytes.Contains(output, []byte("Started at")) {
					t.Fatal("update did not replace and restart:", updateErr, string(output))
				}
			} else if updateErr == nil || !os.SameFile(before, after) || bytes.Contains(output, []byte("Stopped")) {
				t.Fatal("failed update touched installation:", updateErr, string(output))
			}
			token, err := os.ReadFile(path + ".token")
			if err != nil {
				t.Fatal(err)
			}
			if err := control(Config{Listen: address}, string(token), path, "GET", "/health"); err != nil {
				t.Fatal("proxy is not running:", err)
			}
			if _, err := os.Stat(os.Getenv("TEST_INSTALL_MARKER")); !os.IsNotExist(err) {
				t.Fatal("executed an incomplete script")
			}
		})
	}
	brokenRelease := filepath.Join(dir, "broken release")
	if err := os.WriteFile(brokenRelease, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-bridge broken'; else exit 9; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_INSTALL_PAYLOAD", brokenRelease)
	output, err := exec.Command(link, "--config", path, "update").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "binary updated, but proxy restart failed") {
		t.Fatal("restart failure not reported:", err, string(output))
	}
}
