// Package acquireext provides the tool.acquire capability for Pulp cells:
// resolve an external CLI binary to an absolute path, by one of three sources —
// already-installed (PATH), the vendor's official installer, or a manual path.
// A sandboxed WASM cell can't run installers or probe the host filesystem, so
// the host does it here and returns the resolved path.
//
// This is deliberately GENERIC and vendor-neutral: it knows nothing about any
// particular tool, where a caller wants the binary placed, or any application's
// runtime layout. It only answers "given how to get tool X, where is its binary
// now?" The caller (e.g. a workbench that embeds an agent CLI) composes this —
// it calls tool.acquire, then does its own app-specific placement/wiring with
// the returned path. Keeping placement OUT of here is what lets any cell reuse
// the capability without inheriting another app's conventions.
//
// Deployment:
//
//	import _ "github.com/BananaLabs-OSS/Pulp-ext-acquire"
//
// Host imports:
//
//	tool_acquire(req_ptr, req_len, resp_ptr_out, resp_len_out) -> code
//	  req{name, source, install_cmd, path, expect_path}; resp{ok, status, path, message}
package acquireext

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BananaLabs-OSS/Pulp/ext"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"
)

const (
	codeOK          = 0
	codeMemRead     = 2
	codeDecode      = 3
	codeAllocFailed = 7
	codeMemWrite    = 8
	codeCapAbsent   = 99
)

// installTimeout bounds an official-installer run. Installers fetch + unpack a
// binary; generous but not unbounded so a hung script can't wedge the host.
const installTimeout = 10 * time.Minute

var logger = slog.Default()

// SetLogger lets a native caller (a host that uses Resolve directly, outside the
// wasm capability) route this package's logs into its own logger.
func SetLogger(l *slog.Logger) {
	if l != nil {
		logger = l
	}
}

// Request is the input to Resolve. It mirrors the capability's wire request, so
// the same logic serves both the wasm capability and native callers.
type Request struct {
	Name       string // binary to resolve, e.g. "claude"
	Source     string // "installed" | "official" | "manual"
	InstallCmd string // source=official: shell command to run (vendor's official installer)
	Path       string // source=manual: explicit binary path
	ExpectPath string // optional: where the binary lands after install if not on PATH
}

// Result is the resolved outcome. Path is absolute when Ok.
type Result struct {
	Ok      bool
	Status  string // "resolved" | "installed" | "notfound" | "unsupported" | "failed"
	Path    string
	Message string
}

// Resolve runs an acquisition NATIVELY (no wasm) and is the shared core of the
// tool.acquire capability. A host that must run a slow installer off the cell's
// single thread (e.g. a detached orchestrator) calls this directly instead of
// the wasm import. It only RESOLVES — placement is the caller's job.
func Resolve(req Request) Result {
	if req.Name == "" && req.Path == "" {
		return Result{Status: "failed", Message: "acquire: name or path required"}
	}
	switch req.Source {
	case "manual":
		return resolveManual(req)
	case "official":
		return resolveOfficial(req)
	case "installed", "":
		return resolveInstalled(req)
	default:
		return Result{Status: "unsupported", Message: "acquire: unknown source " + req.Source}
	}
}

func init() {
	ext.Register(ext.Capability{
		Name: "tool.acquire",
		Setup: func(env ext.SetupEnv) error {
			if env.Logger != nil {
				logger = env.Logger
			}
			return nil
		},
		Register: bindActive,
		Stub:     bindStub,
	})
}

func bindActive(b wazero.HostModuleBuilder, _ ext.Cell) error {
	b.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
		return toolAcquire(ctx, m, reqPtr, reqLen, respPtrOut, respLenOut)
	}).Export("tool_acquire")
	return nil
}

func bindStub(b wazero.HostModuleBuilder, _ ext.Cell) error {
	b.NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, _, _, _, _ uint32) uint32 { return codeCapAbsent }).Export("tool_acquire")
	return nil
}

type acquireReq struct {
	Name       string `msgpack:"name"`        // binary to resolve, e.g. "claude"
	Source     string `msgpack:"source"`      // "installed" | "official" | "manual"
	InstallCmd string `msgpack:"install_cmd"` // source=official: shell command to run (vendor's official installer)
	Path       string `msgpack:"path"`        // source=manual: explicit binary path
	ExpectPath string `msgpack:"expect_path"` // optional: where the binary lands after install if not on PATH (e.g. "~/.local/bin/claude")
}

type acquireResp struct {
	Ok      bool   `msgpack:"ok"`
	Status  string `msgpack:"status"` // "resolved" | "installed" | "notfound" | "unsupported" | "failed"
	Path    string `msgpack:"path"`   // absolute path to the resolved binary
	Message string `msgpack:"message"`
}

func toolAcquire(ctx context.Context, m api.Module, reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32 {
	var wire acquireReq
	if reqLen > 0 {
		data, ok := m.Memory().Read(reqPtr, reqLen)
		if !ok {
			return codeMemRead
		}
		if err := msgpack.Unmarshal(data, &wire); err != nil {
			return codeDecode
		}
	}
	res := Resolve(Request{
		Name: wire.Name, Source: wire.Source, InstallCmd: wire.InstallCmd,
		Path: wire.Path, ExpectPath: wire.ExpectPath,
	})
	return reply(ctx, m, respPtrOut, respLenOut, acquireResp{
		Ok: res.Ok, Status: res.Status, Path: res.Path, Message: res.Message,
	})
}

// resolveInstalled returns the binary already on PATH (or at an expect_path).
func resolveInstalled(req Request) Result {
	if p := lookup(req.Name); p != "" {
		return Result{Ok: true, Status: "resolved", Path: p, Message: "found on PATH"}
	}
	if p := expectHit(req.ExpectPath); p != "" {
		return Result{Ok: true, Status: "resolved", Path: p, Message: "found at expected path"}
	}
	return Result{Status: "notfound", Message: req.Name + " not on PATH"}
}

// resolveManual verifies a caller-supplied binary path.
func resolveManual(req Request) Result {
	p := expandHome(req.Path)
	if p == "" {
		return Result{Status: "failed", Message: "manual: path required"}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return Result{Status: "failed", Message: "manual: " + err.Error()}
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		return Result{Status: "notfound", Message: "manual: no file at " + abs}
	}
	return Result{Ok: true, Status: "resolved", Path: abs, Message: "manual path verified"}
}

// resolveOfficial runs the vendor's official install command verbatim, then
// re-resolves the binary (PATH, then expect_path). Running the vendor command
// as-is keeps the integrity story theirs, not ours; we record what ran.
func resolveOfficial(req Request) Result {
	if req.InstallCmd == "" {
		return Result{Status: "failed", Message: "official: install_cmd required"}
	}
	// Already there? Skip the install (idempotent).
	if p := lookup(req.Name); p != "" {
		return Result{Ok: true, Status: "resolved", Path: p, Message: "already installed"}
	}
	logger.Info("tool.acquire running official installer", "name", req.Name, "cmd", req.InstallCmd)
	if out, err := runShell(req.InstallCmd); err != nil {
		logger.Error("tool.acquire installer failed", "name", req.Name, "err", err, "out", tailStr(out, 500))
		return Result{Status: "failed", Message: "installer failed: " + err.Error() + "\n" + tailStr(out, 500)}
	}
	if p := lookup(req.Name); p != "" {
		return Result{Ok: true, Status: "installed", Path: p, Message: "installed (on PATH)"}
	}
	if p := expectHit(req.ExpectPath); p != "" {
		return Result{Ok: true, Status: "installed", Path: p, Message: "installed (at expected path)"}
	}
	return Result{Status: "notfound", Message: "installer ran but " + req.Name + " not found on PATH or expect_path"}
}

// runShell runs cmd through the OS's default shell, returning combined output.
// Windows: PowerShell (the form vendor install one-liners use, e.g. `irm … | iex`).
// Unix: sh -c (covers `curl … | sh`).
func runShell(cmd string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
	defer cancel()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmd)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmd)
	}
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	return buf.Bytes(), err
}

// lookup resolves a binary name on PATH to an absolute path ("" if absent).
func lookup(name string) string {
	if name == "" {
		return ""
	}
	if p, err := exec.LookPath(name); err == nil {
		if abs, e := filepath.Abs(p); e == nil {
			return abs
		}
		return p
	}
	return ""
}

// expectHit returns an expanded, existing expect_path ("" if unset/missing).
func expectHit(expect string) string {
	p := expandHome(expect)
	if p == "" {
		return ""
	}
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		if abs, e := filepath.Abs(p); e == nil {
			return abs
		}
		return p
	}
	return ""
}

// expandHome resolves a leading ~ to the user's home dir.
func expandHome(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p[1:], "/"), "\\"))
		}
	}
	return p
}

func tailStr(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// reply marshals v into cell memory; returns codeOK or an alloc/write code.
func reply(ctx context.Context, m api.Module, respPtrOut, respLenOut uint32, v any) uint32 {
	payload, err := msgpack.Marshal(v)
	if err != nil {
		return codeAllocFailed
	}
	return writeResp(ctx, m, payload, respPtrOut, respLenOut)
}

func writeResp(ctx context.Context, m api.Module, data []byte, respPtrOut, respLenOut uint32) uint32 {
	allocFn := m.ExportedFunction("pulp_alloc")
	if allocFn == nil {
		return codeAllocFailed
	}
	res, err := allocFn.Call(ctx, uint64(len(data)))
	if err != nil || len(res) == 0 {
		return codeAllocFailed
	}
	ptr := uint32(res[0])
	if ptr == 0 || !m.Memory().Write(ptr, data) {
		return codeMemWrite
	}
	if !m.Memory().WriteUint32Le(respPtrOut, ptr) || !m.Memory().WriteUint32Le(respLenOut, uint32(len(data))) {
		return codeMemWrite
	}
	return codeOK
}
