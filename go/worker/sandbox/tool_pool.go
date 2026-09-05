package sandbox

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/open-lambda/open-lambda/go/common"
)

//go:embed toolhandler/f.py
var toolHandlerPy []byte

// ToolSandboxPoolConfig configures a short-cycle tool sandbox pool.
type ToolSandboxPoolConfig struct {
	Pool        SandboxPool
	ScratchDirs *common.DirMaker
	// HandlerDir is a host directory with f.py mounted as /handler.
	// If empty, a directory under Worker_dir is created from the embedded stub.
	HandlerDir string
}

type toolSandboxPool struct {
	cfg        *ToolSandboxPoolConfig
	handlerDir string

	mu   sync.Mutex
	idle []*toolSandbox
}

// NewToolSandboxPool creates a ToolSandboxPool backed by an existing SandboxPool (e.g. SOCK).
func NewToolSandboxPool(cfg *ToolSandboxPoolConfig) (ToolSandboxPool, error) {
	if cfg == nil {
		return nil, fmt.Errorf("ToolSandboxPoolConfig is nil")
	}
	if cfg.Pool == nil {
		return nil, fmt.Errorf("ToolSandboxPoolConfig.Pool is nil")
	}
	if cfg.ScratchDirs == nil {
		return nil, fmt.Errorf("ToolSandboxPoolConfig.ScratchDirs is nil")
	}

	handlerDir := cfg.HandlerDir
	if handlerDir == "" {
		handlerDir = filepath.Join(common.Conf.Worker_dir, "tool-handler")
		if err := os.MkdirAll(handlerDir, 0755); err != nil {
			return nil, err
		}
		path := filepath.Join(handlerDir, "f.py")
		if err := os.WriteFile(path, toolHandlerPy, 0644); err != nil {
			return nil, err
		}
	}

	return &toolSandboxPool{
		cfg:        cfg,
		handlerDir: handlerDir,
	}, nil
}

func (p *toolSandboxPool) Acquire(ctx context.Context, meta *ToolMeta) (ToolSandbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if sb := p.takeIdle(); sb != nil {
		if err := sb.Unpause(); err != nil {
			sb.Destroy("tool pool: unpause failed")
			_ = os.RemoveAll(sb.scratchDir)
		} else {
			return sb, nil
		}
	}

	scratchDir := p.cfg.ScratchDirs.Make("tool")
	sbMeta := &SandboxMeta{
		Runtime: common.RT_PYTHON,
	}
	if meta != nil {
		sbMeta.MemLimitMB = meta.MemLimitMB
		sbMeta.CPUPercent = meta.CPUPercent
		sbMeta.Imports = append([]string(nil), meta.Imports...)
	}

	inner, err := p.cfg.Pool.Create(nil, true, p.handlerDir, scratchDir, sbMeta)
	if err != nil {
		_ = os.RemoveAll(scratchDir)
		return nil, fmt.Errorf("tool pool create: %w", err)
	}

	ts, err := newToolSandbox(inner, scratchDir)
	if err != nil {
		inner.Destroy("tool pool: workspace setup failed")
		_ = os.RemoveAll(scratchDir)
		return nil, err
	}
	return ts, nil
}

func (p *toolSandboxPool) Release(sb ToolSandbox, destroy bool) error {
	ts, ok := sb.(*toolSandbox)
	if !ok {
		if sb != nil {
			sb.Destroy("tool pool: unknown sandbox type")
		}
		return fmt.Errorf("Release: not a pool-managed tool sandbox")
	}

	if destroy {
		ts.Destroy("tool pool: release destroy")
		_ = os.RemoveAll(ts.scratchDir)
		return nil
	}

	if err := ts.Pause(); err != nil {
		ts.Destroy("tool pool: pause failed on release")
		_ = os.RemoveAll(ts.scratchDir)
		return err
	}

	p.mu.Lock()
	p.idle = append(p.idle, ts)
	p.mu.Unlock()
	return nil
}

func (p *toolSandboxPool) takeIdle() *toolSandbox {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle) == 0 {
		return nil
	}
	n := len(p.idle) - 1
	sb := p.idle[n]
	p.idle[n] = nil
	p.idle = p.idle[:n]
	return sb
}
