// Package examples holds runnable walkthroughs. This test keeps them honest:
// every example config must parse, validate and compile into a gateway.
package examples

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"prisma/internal/config"
	"prisma/internal/demo"
	"prisma/internal/gateway"
	"prisma/internal/telemetry"
)

// env supplies the secrets the example READMEs ask readers to export.
var env = map[string]string{
	"PRISMA_JWT_SECRET":    "dev-secret-dev-secret-dev-secret!",
	"PRISMA_DEMO_API_KEY":  "demo-key",
	"PRISMA_HMAC_SECRET":   "partner-secret-partner-secret-32b",
	"PRISMA_MASK_HASH_KEY": "change-me",
}

func TestExampleConfigs(t *testing.T) {
	for k, v := range env {
		t.Setenv(k, v)
	}
	// Paths inside the configs are relative to the repository root.
	t.Chdir("..")
	files, err := filepath.Glob("examples/*/prisma.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no example configs found: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(filepath.Dir(file)), func(t *testing.T) {
			cfg, version, err := config.LoadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for i, p := range cfg.Plugins {
				cfg.Plugins[i].File = buildPlugin(t, filepath.Join(filepath.Dir(p.File), "plugin"))
			}
			for i := range cfg.Routes {
				if tc := cfg.Routes[i].GRPCTranscode; tc != nil {
					tc.DescriptorSet = shopDescriptors(t)
				}
			}
			gw := gateway.New(telemetry.NewMetrics(), nil)
			defer gw.Close()
			if err := gw.Apply(cfg, version); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// buildPlugin compiles an example's Proxy-Wasm plugin into a temp dir.
func buildPlugin(t *testing.T, src string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("building Wasm plugins is skipped in -short mode")
	}
	out := filepath.Join(t.TempDir(), "plugin.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", src, err, b)
	}
	return out
}

// shopDescriptors writes the demo descriptor set that the transcoding
// example tells readers to generate.
func shopDescriptors(t *testing.T) string {
	t.Helper()
	b, err := demo.ShopDescriptorSet()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shop.binpb")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
