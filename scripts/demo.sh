#!/usr/bin/env bash
# Reproduce docs/demo.md: build acp-router, run it in a throwaway box
# (own HOME and state under $DEMO_DIR) and drive it with
# scripts/demo_client.py.
#
#   scripts/demo.sh                  testagent phases only (no LLM, no cost)
#   DEMO_FIR_ENV_FILE=~/keys.env scripts/demo.sh
#                                    also one real `fir --mode acp` turn per user
#
# DEMO_FIR_ENV_FILE  KEY=VALUE file with the provider key (e.g.
#                    OPENROUTER_API_KEY=...); read by acp-router as the
#                    users' env_file, never copied into the box.
# DEMO_FIR_PROVIDER  default openrouter
# DEMO_FIR_MODEL     default google/gemini-2.5-flash-lite
# DEMO_DIR           default: a fresh mktemp dir under /tmp (kept, printed)
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
DEMO_DIR=${DEMO_DIR:-$(mktemp -d /tmp/acp-router-demo.XXXXXX)}
bin=$DEMO_DIR/bin
mkdir -p "$bin" "$DEMO_DIR/home"
client=$repo/scripts/demo_client.py

version=$(git -C "$repo" describe --tags --always --dirty)
(cd "$repo" && go build -ldflags "-X main.version=$version" -o "$bin/" ./cmd/acp-router ./internal/testagent/cmd/acp-testagent ./examples/bychannel/cmd/acp-router-bychannel)

# The box: nothing from the caller's HOME is visible to the router or agents.
export HOME=$DEMO_DIR/home
users='"slack:T1:userA": {"max_concurrent_prompts": 1}, "slack:T1:userB": {}'
allow='["slack:T1:userA", "slack:T1:userB"]'

# config <name> <backend json> [users json]
config() {
	cat >"$DEMO_DIR/$1.json" <<EOF
{
  "state_dir": "$DEMO_DIR/$1",
  "audit_log": "$DEMO_DIR/$1/audit.jsonl",
  "backend": $2,
  "policy": {
    "route": "per_user",
    "allow": $allow,
    "ownership": true,
    "thread_cwd": true,
    "max_concurrent_prompts": 2
  },
  "users": {${3:-$users}}
}
EOF
}

# Paths in the output are shown relative to the box.
run() { "$@" 2>&1 | sed "s#$DEMO_DIR#\$DEMO#g"; }

testagent='{"kind": "exec", "command": ["'$bin'/acp-testagent"], "inherit_env": ["PATH"]}'

phase() {
	local name=$1
	echo
	echo "### config $name.json"
	run cat "$DEMO_DIR/$name.json"
	echo
	echo "### client"
	run python3 "$client" "$2" "$3" -config "$DEMO_DIR/$name.json"
	echo
	echo "### state tree"
	(cd "$DEMO_DIR/$name" && find . -mindepth 1 -type d -not -path '*/.*' | sort)
	echo
	echo "### audit.jsonl"
	run cat "$DEMO_DIR/$name/audit.jsonl"
}

echo "box: \$DEMO=$DEMO_DIR (HOME=\$DEMO/home)"
echo "router: $("$bin/acp-router" -version)"

echo; echo "## 1. built-in policy, testagent backends"
config policy "$testagent"
phase policy policy "$bin/acp-router"

echo; echo "## 2. custom policy: examples/bychannel"
config bychannel "$testagent"
phase bychannel bychannel "$bin/acp-router-bychannel"

if [ -n "${DEMO_FIR_ENV_FILE:-}" ]; then
	echo; echo "## 3. real fir --mode acp, one turn per user"
	provider=${DEMO_FIR_PROVIDER:-openrouter}
	model=${DEMO_FIR_MODEL:-google/gemini-2.5-flash-lite}
	f=$(cd "$(dirname "$DEMO_FIR_ENV_FILE")" && pwd)/$(basename "$DEMO_FIR_ENV_FILE")
	fir='{"kind": "exec", "command": ["fir", "--mode", "acp", "--provider", "'$provider'", "--model", "'$model'"], "inherit_env": ["PATH", "LANG"]}'
	config fir "$fir" '"slack:T1:userA": {"env_file": "'"$f"'"}, "slack:T1:userB": {"env_file": "'"$f"'"}'
	phase fir fir "$bin/acp-router" | sed "s#$f#\$DEMO_FIR_ENV_FILE#g"
	echo
	echo "### each user's fir keeps its sessions in its own HOME, keyed by its thread cwd"
	(cd "$DEMO_DIR/fir" && find users -mindepth 5 -maxdepth 5 -type d -path '*/.config/fir/sessions/*' | sort)
fi
