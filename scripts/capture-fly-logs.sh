#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"

usage() {
  cat <<'EOF'
Usage: scripts/capture-fly-logs.sh [output-dir]

Starts a live Fly log capture for this app and writes compact JSON lines to a
timestamped file under log-captures/ by default.

Examples:
  scripts/capture-fly-logs.sh
  scripts/capture-fly-logs.sh /tmp/pressclips-logs

Environment overrides:
  FLY_APP_NAME   Override the Fly app name
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing required command: $1" >&2
    exit 1
  fi
}

require_cmd flyctl
require_cmd jq
require_cmd stdbuf

app_name="${FLY_APP_NAME:-}"
if [[ -z "${app_name}" && -f "${repo_root}/fly.toml" ]]; then
  app_name="$(python3 - <<'PY' "${repo_root}/fly.toml"
import re
import sys

text = open(sys.argv[1], encoding="utf-8").read()
match = re.search(r"^app\s*=\s*['\"]([^'\"]+)['\"]", text, re.MULTILINE)
if match:
    print(match.group(1))
PY
)"
fi

if [[ -z "${app_name}" ]]; then
  echo "Could not determine Fly app name. Set FLY_APP_NAME." >&2
  exit 1
fi

output_dir="${1:-${repo_root}/log-captures}"
mkdir -p "${output_dir}"

timestamp="$(date +%Y%m%d-%H%M%S)"
output_file="${output_dir%/}/fly-search-${timestamp}.jsonl"

finish() {
  echo
  echo "Saved logs to ${output_file}"
}
trap finish EXIT

echo "Capturing live Fly logs for app '${app_name}'"
echo "Output: ${output_file}"
echo "Run the search now, then press Ctrl-C here when it finishes."

stdbuf -oL -eL flyctl logs -a "${app_name}" --json \
  | jq -c . \
  | tee "${output_file}"
