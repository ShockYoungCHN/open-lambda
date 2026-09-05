package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-lambda/open-lambda/go/common"
)

func TestToolSandbox_FileRoundTripAndPathSafety(t *testing.T) {
	scratch := t.TempDir()
	mock := NewMockSandbox("file-test")
	mock.paused = false

	ts, err := newToolSandbox(mock, scratch)
	if err != nil {
		t.Fatal(err)
	}

	if err := ts.WriteFile("a/b.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := ts.ReadFile("a/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q", data)
	}

	entries, err := ts.ListDir("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "b.txt" || entries[0].IsDir {
		t.Fatalf("unexpected list: %+v", entries)
	}

	if _, err := ts.resolve("../etc/passwd"); err == nil {
		t.Fatal("expected path escape error")
	}
	if _, err := ts.resolve("foo/../../etc/passwd"); err == nil {
		t.Fatal("expected nested path escape error")
	}

	if err := ts.Remove("a/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.ReadFile("a/b.txt"); !os.IsNotExist(err) {
		t.Fatalf("expected not exist, got %v", err)
	}
}

func TestToolSandboxPool_AcquireReleaseDestroy(t *testing.T) {
	tmp := t.TempDir()
	common.Conf = &common.Config{
		Worker_dir: tmp,
		Limits: common.LimitsConfig{
			Mem_mb:      50,
			CPU_percent: 100,
			Runtime_sec: 30,
		},
	}

	scratchDirs, err := common.NewDirMaker("scratch", common.STORE_REGULAR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scratchDirs.Cleanup() })

	mockPool := &MockSandboxPool{}
	pool, err := NewToolSandboxPool(&ToolSandboxPoolConfig{
		Pool:        mockPool,
		ScratchDirs: scratchDirs,
	})
	if err != nil {
		t.Fatal(err)
	}

	sb, err := pool.Acquire(context.Background(), &ToolMeta{MemLimitMB: 32})
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile("in.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}

	work := filepath.Join(sb.(*toolSandbox).scratchDir, "workspace", "in.txt")
	if _, err := os.Stat(work); err != nil {
		t.Fatal(err)
	}

	if err := pool.Release(sb, true); err != nil {
		t.Fatal(err)
	}
	if !mockPool.Created[0].IsDestroyed() {
		t.Fatal("expected destroy on release")
	}
}

func TestToolSandboxPool_WarmReleaseReacquire(t *testing.T) {
	tmp := t.TempDir()
	common.Conf = &common.Config{
		Worker_dir: tmp,
		Limits: common.LimitsConfig{
			Mem_mb:      50,
			CPU_percent: 100,
			Runtime_sec: 30,
		},
	}

	scratchDirs, err := common.NewDirMaker("scratch", common.STORE_REGULAR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scratchDirs.Cleanup() })

	mockPool := &MockSandboxPool{}
	pool, err := NewToolSandboxPool(&ToolSandboxPoolConfig{
		Pool:        mockPool,
		ScratchDirs: scratchDirs,
	})
	if err != nil {
		t.Fatal(err)
	}

	sb1, err := pool.Acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	id1 := sb1.ID()
	if err := pool.Release(sb1, false); err != nil {
		t.Fatal(err)
	}
	if !mockPool.Created[0].IsPaused() {
		t.Fatal("expected paused after warm release")
	}

	sb2, err := pool.Acquire(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sb2.ID() != id1 {
		t.Fatalf("expected warm reacquire of %s, got %s", id1, sb2.ID())
	}
	if mockPool.Created[0].IsPaused() {
		t.Fatal("expected unpaused after reacquire")
	}
	if len(mockPool.Created) != 1 {
		t.Fatalf("expected 1 create, got %d", len(mockPool.Created))
	}

	_ = pool.Release(sb2, true)
}

func TestToolSandbox_ExecRequiresCmd(t *testing.T) {
	ts, err := newToolSandbox(NewMockSandbox("x"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts.Exec(context.Background(), ExecRequest{Timeout: time.Second})
	if err == nil {
		t.Fatal("expected error for empty cmd")
	}
}
