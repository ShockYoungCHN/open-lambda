# Stub handler for tool sandboxes. Use POST /tool/exec for structured commands.
def f(event):
    return {"ok": True, "hint": "use /tool/exec"}
