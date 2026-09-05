package sandbox

import (
	"context"
	"time"
)

// NetworkPolicy controls outbound network for a tool sandbox.
// Enforcement may be incomplete in early versions; the field is part of the API.
type NetworkPolicy int

const (
	// NetworkDefault uses the platform default (typically whatever the container already has).
	NetworkDefault NetworkPolicy = iota
	// NetworkDenyAll requests no outbound network (best-effort until enforced).
	NetworkDenyAll
)

// ExecRequest is a one-shot command execution inside a tool sandbox.
type ExecRequest struct {
	Cmd     []string
	Cwd     string            // container path; empty means /host/workspace
	Env     map[string]string // merged over a minimal env; does not inherit the host
	Stdin   []byte
	Timeout time.Duration
}

// ExecResult is the outcome of Exec.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
}

// FileInfo is a directory entry from ListDir.
type FileInfo struct {
	Name  string
	IsDir bool
	Size  int64
}

// ToolMeta configures Acquire for a short-cycle tool sandbox.
type ToolMeta struct {
	MemLimitMB int
	CPUPercent int
	Imports    []string
	Network    NetworkPolicy
}

// ToolSandbox is a short-cycle agent tool sandbox: structured Exec plus scratch file I/O,
// on top of the existing Sandbox lifecycle (Pause / Unpause / Destroy).
type ToolSandbox interface {
	Sandbox

	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)

	// File ops are rooted at the sandbox scratch workspace (host-side bind of /host/workspace).
	WriteFile(relPath string, data []byte) error
	ReadFile(relPath string) ([]byte, error)
	ListDir(relPath string) ([]FileInfo, error)
	Remove(relPath string) error
}

// ToolSandboxPool acquires short-lived tool sandboxes.
// Release(..., true) destroys; Release(..., false) pauses and may return the sandbox to a warm pool.
type ToolSandboxPool interface {
	Acquire(ctx context.Context, meta *ToolMeta) (ToolSandbox, error)
	Release(sb ToolSandbox, destroy bool) error
}
