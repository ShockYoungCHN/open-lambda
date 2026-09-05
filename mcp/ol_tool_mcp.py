#!/usr/bin/env python3
"""Agent interaction interface for OpenLambda short-cycle tool sandboxes (MCP).

This is a command/fs API for agents — not an interactive PTY/terminal.
"""

from __future__ import annotations

import json
import os
from typing import Any

import httpx
from mcp.server.mcpserver import MCPServer

OL_TOOL_BASE = os.environ.get("OL_TOOL_BASE", "http://127.0.0.1:5000").rstrip("/")
DEFAULT_TIMEOUT = float(os.environ.get("OL_TOOL_TIMEOUT", "120"))

AGENT_INSTRUCTIONS = """
OpenLambda agent sandbox interface (short-cycle, non-interactive).

Use this for coding tasks that should run isolated from the host:
1) sb_start → keep session_id
2) sb_write / sb_read / sb_list to manage workspace files
3) sb_exec to run one-shot shell commands (bash -lc), not an interactive terminal
4) sb_close when the task is done

Rules:
- Pass the same session_id to every call in one task.
- Prefer many small sb_exec calls over pretending you have a PTY (no vim, no prompts).
- Working directory for sb_exec is the sandbox workspace.
- If installs fail (e.g. missing compiler), report the error; do not assume apt/gcc exist.
""".strip()

mcp = MCPServer(
    name="openlambda-tool-sandbox",
    instructions=AGENT_INSTRUCTIONS,
)


def _url(path: str) -> str:
    return f"{OL_TOOL_BASE}{path}"


def _client() -> httpx.Client:
    return httpx.Client(timeout=DEFAULT_TIMEOUT)


def _err(resp: httpx.Response) -> str:
    try:
        data = resp.json()
        if isinstance(data, dict) and data.get("error"):
            return str(data["error"])
    except Exception:
        pass
    return f"HTTP {resp.status_code}: {resp.text[:500]}"


@mcp.tool()
def sb_start(mem_limit_mb: int = 256, imports: str = "") -> str:
    """Start a sandbox session for one coding task.

    Returns JSON with session_id. Call once at task start; reuse session_id
    for sb_write/sb_read/sb_list/sb_exec/sb_close.

    imports: optional comma-separated Python modules to pre-import (e.g. "json,os").
    """
    body: dict[str, Any] = {"mem_limit_mb": mem_limit_mb}
    if imports.strip():
        body["imports"] = [x.strip() for x in imports.split(",") if x.strip()]
    with _client() as client:
        r = client.post(_url("/tool/session"), json=body)
    if r.status_code >= 400:
        return f"sb_start failed: {_err(r)}"
    return json.dumps(r.json())


@mcp.tool()
def sb_write(session_id: str, path: str, content: str) -> str:
    """Write a text file into the sandbox workspace (relative path)."""
    with _client() as client:
        r = client.post(
            _url(f"/tool/session/{session_id}/write"),
            json={"path": path, "content": content},
        )
    if r.status_code >= 400:
        return f"sb_write failed: {_err(r)}"
    return json.dumps(r.json())


@mcp.tool()
def sb_read(session_id: str, path: str) -> str:
    """Read a text file from the sandbox workspace."""
    with _client() as client:
        r = client.post(
            _url(f"/tool/session/{session_id}/read"),
            json={"path": path},
        )
    if r.status_code >= 400:
        return f"sb_read failed: {_err(r)}"
    data = r.json()
    return data.get("content", json.dumps(data))


@mcp.tool()
def sb_list(session_id: str, path: str = ".") -> str:
    """List files in a sandbox workspace directory."""
    with _client() as client:
        r = client.post(
            _url(f"/tool/session/{session_id}/list"),
            json={"path": path},
        )
    if r.status_code >= 400:
        return f"sb_list failed: {_err(r)}"
    return json.dumps(r.json(), indent=2)


@mcp.tool()
def sb_exec(session_id: str, command: str, timeout_ms: int = 30000) -> str:
    """Run one non-interactive shell command in the sandbox workspace (bash -lc).

    This is a command-execution interface, not an interactive terminal/PTY.
    Examples: "python hello.py", "pytest -q", "ls -la".
    """
    with _client() as client:
        r = client.post(
            _url(f"/tool/session/{session_id}/exec"),
            json={
                "cmd": ["bash", "-lc", command],
                "timeout_ms": timeout_ms,
            },
        )
    if r.status_code >= 400:
        return f"sb_exec failed: {_err(r)}"
    data = r.json()
    return "\n".join(
        [
            f"exit_code={data.get('exit_code')}",
            f"timed_out={data.get('timed_out')}",
            "--- stdout ---",
            data.get("stdout") or "",
            "--- stderr ---",
            data.get("stderr") or "",
        ]
    )


@mcp.tool()
def sb_remove(session_id: str, path: str) -> str:
    """Remove a file or directory from the sandbox workspace."""
    with _client() as client:
        r = client.post(
            _url(f"/tool/session/{session_id}/remove"),
            json={"path": path},
        )
    if r.status_code >= 400:
        return f"sb_remove failed: {_err(r)}"
    return json.dumps(r.json())


@mcp.tool()
def sb_close(session_id: str, destroy: bool = True) -> str:
    """End the sandbox session. destroy=True (default) tears it down; False pauses for reuse."""
    with _client() as client:
        r = client.post(
            _url(f"/tool/session/{session_id}/release"),
            json={"destroy": destroy},
        )
    if r.status_code >= 400:
        return f"sb_close failed: {_err(r)}"
    return json.dumps(r.json())


def main() -> None:
    mcp.run(transport="stdio")


if __name__ == "__main__":
    main()
