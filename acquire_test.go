package acquireext

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// resolveInstalled finds a binary that's actually on PATH, and reports notfound
// for one that isn't.
func TestResolveInstalled(t *testing.T) {
	// `go` is on PATH wherever the tests run.
	got := resolveInstalled(Request{Name: "go"})
	if !got.Ok || got.Status != "resolved" {
		t.Fatalf("installed go: got %+v", got)
	}
	if !filepath.IsAbs(got.Path) {
		t.Fatalf("installed path not absolute: %q", got.Path)
	}

	miss := resolveInstalled(Request{Name: "definitely-not-a-real-binary-xyzzy"})
	if miss.Ok || miss.Status != "notfound" {
		t.Fatalf("missing binary should be notfound, got %+v", miss)
	}
}

// resolveInstalled falls back to expect_path when the name isn't on PATH.
func TestResolveInstalledExpectPath(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "fakeagent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := resolveInstalled(Request{Name: "definitely-not-on-path-xyzzy", ExpectPath: bin})
	if !got.Ok || got.Status != "resolved" || got.Path != bin {
		t.Fatalf("expect_path fallback: got %+v want path %q", got, bin)
	}
}

// resolveManual verifies a real path and rejects a bogus one / a directory.
func TestResolveManual(t *testing.T) {
	gopath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH")
	}
	ok := resolveManual(Request{Path: gopath})
	if !ok.Ok || ok.Status != "resolved" {
		t.Fatalf("manual real path: got %+v", ok)
	}

	bad := resolveManual(Request{Path: filepath.Join(t.TempDir(), "nope")})
	if bad.Ok || bad.Status != "notfound" {
		t.Fatalf("manual bogus path: got %+v", bad)
	}

	dir := resolveManual(Request{Path: t.TempDir()})
	if dir.Ok {
		t.Fatalf("manual dir should fail, got %+v", dir)
	}

	empty := resolveManual(Request{Path: ""})
	if empty.Ok || empty.Status != "failed" {
		t.Fatalf("manual empty path: got %+v", empty)
	}
}

// official with no install_cmd fails fast; with the binary already present it
// short-circuits to resolved without running anything.
func TestResolveOfficial(t *testing.T) {
	noCmd := resolveOfficial(Request{Name: "go"})
	if noCmd.Status != "failed" {
		t.Fatalf("official without install_cmd should fail, got %+v", noCmd)
	}
	// `go` is already installed → short-circuit, installer never runs.
	pre := resolveOfficial(Request{Name: "go", InstallCmd: "exit 1"})
	if !pre.Ok || pre.Status != "resolved" {
		t.Fatalf("official with pre-installed binary should short-circuit, got %+v", pre)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandHome("~/foo"); got != filepath.Join(home, "foo") {
		t.Fatalf("expandHome ~/foo = %q want %q", got, filepath.Join(home, "foo"))
	}
	if got := expandHome("~"); got != home {
		t.Fatalf("expandHome ~ = %q want %q", got, home)
	}
	// No tilde → unchanged.
	plain := filepath.Join("a", "b")
	if got := expandHome(plain); got != plain {
		t.Fatalf("expandHome %q = %q (should be unchanged)", plain, got)
	}
}

// runShell runs a trivial command through the OS shell and captures output.
func TestRunShell(t *testing.T) {
	cmd := "echo acquire_shell_ok"
	if runtime.GOOS == "windows" {
		cmd = "Write-Output acquire_shell_ok"
	}
	out, err := runShell(cmd)
	if err != nil {
		t.Fatalf("runShell err: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "acquire_shell_ok") {
		t.Fatalf("runShell output missing marker: %q", out)
	}
}
