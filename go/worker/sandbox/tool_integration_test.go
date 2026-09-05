//go:build integration

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-lambda/open-lambda/go/common"
)

// Smoke: WriteFile → Exec → ReadFile → Destroy against a real Docker ol-min sandbox.
func TestIntegration_ToolSandbox_WriteExecReadDestroy(t *testing.T) {
	tmpDir := t.TempDir()
	workerDir := filepath.Join(tmpDir, "worker")
	pkgsDir := filepath.Join(tmpDir, "packages")
	for _, d := range []string{workerDir, pkgsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	common.Conf = &common.Config{
		Worker_dir: workerDir,
		Pkgs_dir:   pkgsDir,
		Sandbox:    "docker",
		Docker: common.DockerConfig{
			Base_image: "ol-min",
		},
		Limits: common.LimitsConfig{
			Procs:       10,
			Mem_mb:      128,
			CPU_percent: 100,
			Swappiness:  0,
			Runtime_sec: 60,
		},
		Features: common.FeaturesConfig{
			Enable_seccomp: false,
		},
	}

	sbPool, err := NewDockerPool("", nil)
	if err != nil {
		t.Fatalf("NewDockerPool: %v (is Docker running? is ol-min built?)", err)
	}
	t.Cleanup(sbPool.Cleanup)

	scratchDirs, err := common.NewDirMaker("scratch", common.STORE_REGULAR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scratchDirs.Cleanup() })

	pool, err := NewToolSandboxPool(&ToolSandboxPoolConfig{
		Pool:        sbPool,
		ScratchDirs: scratchDirs,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb, err := pool.Acquire(ctx, &ToolMeta{MemLimitMB: 128})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if err := sb.WriteFile("input.txt", []byte("hello-tool")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	res, err := sb.Exec(ctx, ExecRequest{
		Cmd:     []string{"bash", "-lc", "cat input.txt > output.txt && echo ok"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.TimedOut {
		t.Fatalf("Exec timed out: stderr=%q", res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "ok") {
		t.Fatalf("unexpected stdout %q", res.Stdout)
	}

	out, err := sb.ReadFile("output.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(out) != "hello-tool" {
		t.Fatalf("output.txt=%q", out)
	}

	if err := pool.Release(sb, true); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
