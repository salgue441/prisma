package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	wasmOnce sync.Once
	wasmPath string
	wasmErr  error
)

// WasmPlugin builds internal/wasm/testdata/plugin (a Proxy-Wasm plugin using
// the official Go SDK) once per test binary and returns the .wasm path.
func WasmPlugin(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available to build the test plugin")
	}
	wasmOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		src := filepath.Join(filepath.Dir(file), "..", "wasm", "testdata", "plugin")
		dir, err := os.MkdirTemp("", "prisma-wasm")
		if err != nil {
			wasmErr = err
			return
		}
		wasmPath = filepath.Join(dir, "plugin.wasm")
		cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", wasmPath, ".")
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		if out, err := cmd.CombinedOutput(); err != nil {
			wasmErr = &buildError{err: err, out: string(out)}
		}
	})
	if wasmErr != nil {
		t.Fatal(wasmErr)
	}
	return wasmPath
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return "building test plugin: " + e.err.Error() + "\n" + e.out }
