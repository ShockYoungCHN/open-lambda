package event

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-lambda/open-lambda/go/common"
	"github.com/open-lambda/open-lambda/go/worker/sandbox"
)

const (
	toolSessionPath = "/tool/session"
	maxToolBody     = 8 << 20 // 8 MiB
)

var nextToolSession uint64

// ToolAPI exposes short-cycle ToolSandbox over HTTP for MCP / agent bridges.
type ToolAPI struct {
	pool sandbox.ToolSandboxPool

	mu       sync.Mutex
	sessions map[string]sandbox.ToolSandbox
}

// NewToolAPI builds a ToolAPI on top of an existing SandboxPool.
func NewToolAPI(sbPool sandbox.SandboxPool) (*ToolAPI, error) {
	scratchMode := common.STORE_REGULAR
	if common.Conf.Storage.Scratch != "" {
		scratchMode = common.Conf.Storage.Scratch.Mode()
	}

	scratchDirs, err := common.NewDirMaker("tool-scratch", scratchMode)
	if err != nil {
		return nil, fmt.Errorf("tool-scratch dirs: %w", err)
	}

	pool, err := sandbox.NewToolSandboxPool(&sandbox.ToolSandboxPoolConfig{
		Pool:        sbPool,
		ScratchDirs: scratchDirs,
	})
	if err != nil {
		return nil, err
	}

	return &ToolAPI{
		pool:     pool,
		sessions: make(map[string]sandbox.ToolSandbox),
	}, nil
}

// Register mounts tool HTTP handlers on mux.
func (api *ToolAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc(toolSessionPath, api.handleSessionCollection)
	mux.HandleFunc(toolSessionPath+"/", api.handleSessionItem)
	slog.Info("Tool API ready", "paths", []string{
		"POST /tool/session",
		"POST /tool/session/{id}/exec",
		"POST /tool/session/{id}/write",
		"POST /tool/session/{id}/read",
		"POST /tool/session/{id}/list",
		"POST /tool/session/{id}/remove",
		"POST /tool/session/{id}/release",
		"DELETE /tool/session/{id}",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, maxToolBody))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func (api *ToolAPI) handleSessionCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}

	var req struct {
		MemLimitMB int      `json:"mem_limit_mb"`
		CPUPercent int      `json:"cpu_percent"`
		Imports    []string `json:"imports"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil && err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}

	meta := &sandbox.ToolMeta{
		MemLimitMB: req.MemLimitMB,
		CPUPercent: req.CPUPercent,
		Imports:    req.Imports,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	sb, err := api.pool.Acquire(ctx, meta)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	id := fmt.Sprintf("ts-%d", atomic.AddUint64(&nextToolSession, 1))
	api.mu.Lock()
	api.sessions[id] = sb
	api.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{"session_id": id})
}

func (api *ToolAPI) getSession(id string) (sandbox.ToolSandbox, bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	sb, ok := api.sessions[id]
	return sb, ok
}

func (api *ToolAPI) takeSession(id string) (sandbox.ToolSandbox, bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	sb, ok := api.sessions[id]
	if ok {
		delete(api.sessions, id)
	}
	return sb, ok
}

func (api *ToolAPI) handleSessionItem(w http.ResponseWriter, r *http.Request) {
	// /tool/session/{id}/...
	path := strings.TrimPrefix(r.URL.Path, toolSessionPath+"/")
	path = strings.Trim(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "missing session id"})
		return
	}

	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	if action == "" {
		if r.Method == http.MethodDelete {
			api.releaseSession(w, r, id, true)
			return
		}
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "DELETE or /{action}"})
		return
	}

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}

	switch action {
	case "exec":
		api.handleExec(w, r, id)
	case "write":
		api.handleWrite(w, r, id)
	case "read":
		api.handleRead(w, r, id)
	case "list":
		api.handleList(w, r, id)
	case "remove":
		api.handleRemove(w, r, id)
	case "release":
		var req struct {
			Destroy *bool `json:"destroy"`
		}
		_ = readJSON(r, &req)
		destroy := true
		if req.Destroy != nil {
			destroy = *req.Destroy
		}
		api.releaseSession(w, r, id, destroy)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown action: " + action})
	}
}

func (api *ToolAPI) releaseSession(w http.ResponseWriter, _ *http.Request, id string, destroy bool) {
	sb, ok := api.takeSession(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
		return
	}
	if err := api.pool.Release(sb, destroy); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "destroyed": destroy})
}

func (api *ToolAPI) handleExec(w http.ResponseWriter, r *http.Request, id string) {
	sb, ok := api.getSession(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
		return
	}

	var req struct {
		Cmd       []string          `json:"cmd"`
		Cwd       string            `json:"cwd"`
		Env       map[string]string `json:"env"`
		Stdin     string            `json:"stdin"`
		StdinB64  string            `json:"stdin_b64"`
		TimeoutMs int64             `json:"timeout_ms"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	stdin := []byte(req.Stdin)
	if req.StdinB64 != "" {
		b, err := base64.StdEncoding.DecodeString(req.StdinB64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad stdin_b64"})
			return
		}
		stdin = b
	}

	timeout := 30 * time.Second
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout+5*time.Second)
	defer cancel()

	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Cmd:     req.Cmd,
		Cwd:     req.Cwd,
		Env:     req.Env,
		Stdin:   stdin,
		Timeout: timeout,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code":  res.ExitCode,
		"stdout":     string(res.Stdout),
		"stderr":     string(res.Stderr),
		"stdout_b64": base64.StdEncoding.EncodeToString(res.Stdout),
		"stderr_b64": base64.StdEncoding.EncodeToString(res.Stderr),
		"timed_out":  res.TimedOut,
	})
}

func (api *ToolAPI) handleWrite(w http.ResponseWriter, r *http.Request, id string) {
	sb, ok := api.getSession(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
		return
	}

	var req struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		ContentB64 string `json:"content_b64"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path required"})
		return
	}

	data := []byte(req.Content)
	if req.ContentB64 != "" {
		b, err := base64.StdEncoding.DecodeString(req.ContentB64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad content_b64"})
			return
		}
		data = b
	}

	if err := sb.WriteFile(req.Path, data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bytes": len(data)})
}

func (api *ToolAPI) handleRead(w http.ResponseWriter, r *http.Request, id string) {
	sb, ok := api.getSession(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	data, err := sb.ReadFile(req.Path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":        req.Path,
		"content":     string(data),
		"content_b64": base64.StdEncoding.EncodeToString(data),
		"bytes":       len(data),
	})
}

func (api *ToolAPI) handleList(w http.ResponseWriter, r *http.Request, id string) {
	sb, ok := api.getSession(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	_ = readJSON(r, &req)

	entries, err := sb.ListDir(req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (api *ToolAPI) handleRemove(w http.ResponseWriter, r *http.Request, id string) {
	sb, ok := api.getSession(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown session"})
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := sb.Remove(req.Path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
