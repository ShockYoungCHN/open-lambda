package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	toolWorkspaceHostSubdir = "workspace"
	toolWorkspaceGuestPath  = "/host/workspace"
	toolExecPath            = "/tool/exec"
	defaultExecTimeout      = 30 * time.Second
	maxToolOutputBytes      = 1 << 20 // 1 MiB; mirrored in runtime
)

// toolSandbox wraps a Sandbox with scratch-rooted file APIs and /tool/exec.
type toolSandbox struct {
	Sandbox
	scratchDir string
	workDir    string
}

func newToolSandbox(sb Sandbox, scratchDir string) (*toolSandbox, error) {
	workDir := filepath.Join(scratchDir, toolWorkspaceHostSubdir)
	if err := os.MkdirAll(workDir, 0777); err != nil {
		return nil, fmt.Errorf("tool sandbox workspace: %w", err)
	}
	return &toolSandbox{
		Sandbox:    sb,
		scratchDir: scratchDir,
		workDir:    workDir,
	}, nil
}

func (t *toolSandbox) resolve(relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("empty path")
	}
	// Reject ".." components before Clean so callers get a clear error
	// instead of a silently rewritten path under the workspace.
	for _, part := range strings.Split(filepath.ToSlash(relPath), "/") {
		if part == ".." {
			return "", fmt.Errorf("path escapes workspace: %q", relPath)
		}
	}
	cleaned := filepath.Clean("/" + relPath)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return t.workDir, nil
	}

	full := filepath.Join(t.workDir, cleaned)
	rel, err := filepath.Rel(t.workDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace: %q", relPath)
	}
	return full, nil
}

func (t *toolSandbox) WriteFile(relPath string, data []byte) error {
	full, err := t.resolve(relPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0777); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0666)
}

func (t *toolSandbox) ReadFile(relPath string) ([]byte, error) {
	full, err := t.resolve(relPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (t *toolSandbox) ListDir(relPath string) ([]FileInfo, error) {
	if relPath == "" {
		relPath = "."
	}
	full, err := t.resolve(relPath)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, FileInfo{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Size:  info.Size(),
		})
	}
	return out, nil
}

func (t *toolSandbox) Remove(relPath string) error {
	full, err := t.resolve(relPath)
	if err != nil {
		return err
	}
	if full == t.workDir {
		return fmt.Errorf("cannot remove workspace root")
	}
	return os.RemoveAll(full)
}

type toolExecWireReq struct {
	Cmd       []string          `json:"cmd"`
	Cwd       string            `json:"cwd"`
	Env       map[string]string `json:"env,omitempty"`
	StdinB64  string            `json:"stdin_b64,omitempty"`
	TimeoutMs int64             `json:"timeout_ms"`
}

type toolExecWireResp struct {
	ExitCode  int    `json:"exit_code"`
	StdoutB64 string `json:"stdout_b64"`
	StderrB64 string `json:"stderr_b64"`
	TimedOut  bool   `json:"timed_out"`
	Error     string `json:"error,omitempty"`
}

func (t *toolSandbox) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if len(req.Cmd) == 0 {
		return nil, fmt.Errorf("Exec: empty cmd")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cwd := req.Cwd
	if cwd == "" {
		cwd = toolWorkspaceGuestPath
	}

	wire := toolExecWireReq{
		Cmd:       req.Cmd,
		Cwd:       cwd,
		Env:       req.Env,
		TimeoutMs: timeout.Milliseconds(),
	}
	if len(req.Stdin) > 0 {
		wire.StdinB64 = base64.StdEncoding.EncodeToString(req.Stdin)
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://root"+toolExecPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.ContentLength = int64(len(body))

	client := t.Client()
	if client == nil {
		return nil, fmt.Errorf("Exec: sandbox has no HTTP client")
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Exec: round trip: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxToolOutputBytes*4))
	if err != nil {
		return nil, fmt.Errorf("Exec: read response: %w", err)
	}

	var wireResp toolExecWireResp
	if err := json.Unmarshal(respBody, &wireResp); err != nil {
		return nil, fmt.Errorf("Exec: bad response (status=%d): %w; body=%q", resp.StatusCode, err, truncateForErr(respBody))
	}
	if wireResp.Error != "" && resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Exec: %s", wireResp.Error)
	}

	stdout, err := decodeB64Optional(wireResp.StdoutB64)
	if err != nil {
		return nil, fmt.Errorf("Exec: stdout: %w", err)
	}
	stderr, err := decodeB64Optional(wireResp.StderrB64)
	if err != nil {
		return nil, fmt.Errorf("Exec: stderr: %w", err)
	}

	return &ExecResult{
		ExitCode: wireResp.ExitCode,
		Stdout:   stdout,
		Stderr:   stderr,
		TimedOut: wireResp.TimedOut,
	}, nil
}

func decodeB64Optional(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

func truncateForErr(b []byte) string {
	const n = 256
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
