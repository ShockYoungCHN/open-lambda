# OpenLambda agent sandbox MCP

Agent-facing interaction interface for short-cycle OpenLambda tool sandboxes.

**Not an interactive terminal/PTY.** Agents get `session + fs + exec`.

## Prerequisites

1. OpenLambda worker running (`sudo ./ol worker up -p myworker -d`), port **5000**.
2. MCP venv (below).

## Setup

```bash
cd /home/yancho/open-lambda/mcp
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

Config: [`.cursor/mcp.json`](../.cursor/mcp.json) and/or `~/.cursor/mcp.json`.  
Reload Cursor MCP after changes.

## Agent tools

| Tool | Purpose |
|---|---|
| `sb_start` | Open session → `session_id` |
| `sb_write` | Write workspace file |
| `sb_read` | Read workspace file |
| `sb_list` | List directory |
| `sb_exec` | One-shot `bash -lc` command (non-interactive) |
| `sb_remove` | Delete path |
| `sb_close` | Release session |

Typical loop: `sb_start` → (`sb_write` / `sb_exec` / `sb_read`)* → `sb_close`.

## Example prompt

> Use the OpenLambda sandbox tools: sb_start, write hello.py that prints hi, sb_exec to run it, then sb_close.

## HTTP (worker)

Same semantics under `/tool/session/...` (unchanged).
