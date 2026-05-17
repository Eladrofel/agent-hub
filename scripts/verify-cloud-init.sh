#!/usr/bin/env bash
# verify-cloud-init.sh — render cloud-init/user-data.yaml.tpl with sample
# values via `tofu console` (or `terraform console`), strip the console's
# string-quoting, and validate the result parses as YAML.
#
# Catches the indent() whitespace-bug class (where an embedded block scalar
# gets malformed) BEFORE `tofu apply` pushes a broken user-data to the VM.
#
# Exit codes:
#   0 — rendered cloud-init is valid YAML
#   1 — rendered cloud-init is malformed (or rendering itself failed)
#
# Run from the repo root: ./scripts/verify-cloud-init.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Prefer tofu, fall back to terraform.
if command -v tofu >/dev/null 2>&1; then
  TF=tofu
elif command -v terraform >/dev/null 2>&1; then
  TF=terraform
else
  echo "verify-cloud-init: neither tofu nor terraform on PATH" >&2
  exit 1
fi

# Render the template with stub values. docker_compose_content uses the real
# docker-compose.yml so the indent() interaction is exercised against actual
# content (not a one-liner stub that would mask the bug).
RENDER_EXPR='templatefile("cloud-init/user-data.yaml.tpl", {
  hostname               = "test-host",
  username               = "test-user",
  ssh_key                = "ssh-rsa AAAAB3NzaC1yc2EAAAA test@example",
  docker_compose_content = file("docker-compose.yml"),
})'

# `tofu console` returns the value as a Go-quoted string on stdout. We need
# the raw YAML. Pipe through python to handle the unquoting + YAML parse
# in one step — much more robust than sed for backslash-escape handling.
RENDERED="$(printf '%s\n' "$RENDER_EXPR" | "$TF" console 2>/dev/null || true)"

if [ -z "$RENDERED" ]; then
  echo "verify-cloud-init: tofu/terraform console produced no output (init missing?)" >&2
  exit 1
fi

# Ensure PyYAML is importable; transparently install to the user site if not.
# (Skipped on CI runners that pre-provision pyyaml.)
if ! python3 -c 'import yaml' >/dev/null 2>&1; then
  python3 -m pip install --user --quiet --disable-pip-version-check pyyaml \
    >/dev/null 2>&1 \
    || python3 -m pip install --user --quiet --disable-pip-version-check \
                                --break-system-packages pyyaml >/dev/null 2>&1 \
    || { echo "verify-cloud-init: could not import or install PyYAML" >&2; exit 1; }
fi

RENDERED="$RENDERED" python3 - <<'PYEOF'
import json, os, sys, yaml

raw = os.environ["RENDERED"]
stripped = raw.strip()

# tofu console emits one of two representations:
#   1. A JSON-string (double-quoted, with \n and \" escapes) — for short strings.
#   2. An HCL heredoc: starts with `<<EOT\n` or `<<-EOT\n` and ends with `EOT`.
# Detect which and unwrap accordingly.
if stripped.startswith('"') and stripped.endswith('"'):
    try:
        rendered_yaml = json.loads(stripped)
    except Exception as e:
        print(f"verify-cloud-init: could not unquote JSON-string output: {e}", file=sys.stderr)
        sys.exit(1)
elif stripped.startswith("<<"):
    # First line: `<<EOT` or `<<-EOT`; last non-empty line: matching delimiter.
    lines = stripped.splitlines()
    header = lines[0]
    delim = header.lstrip("<-").strip()
    # Drop header; find the closing delim line (last occurrence).
    body_lines = lines[1:]
    for i in range(len(body_lines) - 1, -1, -1):
        if body_lines[i].strip() == delim:
            body_lines = body_lines[:i]
            break
    rendered_yaml = "\n".join(body_lines) + "\n"
else:
    print("verify-cloud-init: unrecognised tofu console output format", file=sys.stderr)
    print("--- raw (first 5 lines) ---", file=sys.stderr)
    for line in stripped.splitlines()[:5]:
        print(line, file=sys.stderr)
    sys.exit(1)

try:
    yaml.safe_load(rendered_yaml)
except yaml.YAMLError as e:
    print("verify-cloud-init: rendered cloud-init is NOT valid YAML", file=sys.stderr)
    print(f"  {e}", file=sys.stderr)
    print("--- rendered (first 80 lines) ---", file=sys.stderr)
    for i, line in enumerate(rendered_yaml.splitlines()[:80], 1):
        print(f"{i:4d}: {line}", file=sys.stderr)
    sys.exit(1)

print("✓ cloud-init renders to valid YAML")
PYEOF
