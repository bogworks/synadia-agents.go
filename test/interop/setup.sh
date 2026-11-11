#!/bin/sh
# Copyright 2026 The synadia-agents.go Authors
# SPDX-License-Identifier: Apache-2.0
#
# Fetch and build the official Synadia Agents SDKs (TypeScript and Python) at the
# commit pinned in PINNED, for the interop tests (AT-003). Idempotent: a second
# run with a finished cache only prints the environment.
#
#   SAP_INTEROP_CACHE   cache root (default: $HOME/.cache/synadia-agents-interop)
#   SAP_INTEROP_SOURCE  git URL or path to fetch from instead of UPSTREAM_REPO
#   SAP_INTEROP_FORCE=1 rebuild even if the cache is marked ready
#
# Everything is written under <cache>/<sha>/, never inside this repository.
set -eu

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

pin() { sed -n "s/^$1=//p" "$here/PINNED" | head -n 1; }

sha=$(pin UPSTREAM_SHA)
repo=${SAP_INTEROP_SOURCE:-$(pin UPSTREAM_REPO)}
pyver=$(pin PYTHON_VERSION)
node_min=$(pin NODE_MIN_MAJOR)
if [ -z "$sha" ] || [ -z "$pyver" ] || [ -z "$node_min" ]; then
	echo "setup.sh: PINNED is incomplete" >&2
	exit 1
fi

cache=${SAP_INTEROP_CACHE:-$HOME/.cache/synadia-agents-interop}
root=$cache/$sha
src=$root/src
venv=$root/venv

print_env() {
	echo "export SAP_INTEROP_CACHE=$cache"
	echo "# upstream checkout: $src"
	echo "# runtime versions: $root/versions.txt"
}

if [ -f "$root/ready" ] && [ "${SAP_INTEROP_FORCE:-}" != 1 ]; then
	echo "interop cache ready: $root"
	print_env
	exit 0
fi

need() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "setup.sh: $1 is required but not on PATH" >&2
		exit 1
	}
}
need git
need node
need npm
need uv

node_major=$(node -p 'process.versions.node.split(".")[0]')
if [ "$node_major" -lt "$node_min" ]; then
	echo "setup.sh: node $node_major found, >= $node_min required" >&2
	exit 1
fi

mkdir -p "$root"
rm -f "$root/ready"

# 1. Source at the pinned commit.
if [ ! -d "$src/.git" ]; then
	git init -q "$src"
	git -C "$src" remote add origin "$repo"
fi
if [ "$(git -C "$src" rev-parse -q --verify HEAD 2>/dev/null || true)" != "$sha" ]; then
	git -C "$src" fetch -q --depth 1 origin "$sha"
	git -C "$src" checkout -q --detach -f "$sha"
fi
if [ "$(git -C "$src" rev-parse HEAD)" != "$sha" ]; then
	echo "setup.sh: checkout is not $sha" >&2
	exit 1
fi

# 2. TypeScript SDKs. The upstream lockfiles are bun's, which npm does not read, so
# this repository commits npm lockfiles under locks/ (ts-client, ts-agent) and
# `npm ci` installs exactly those versions and integrity hashes; nothing resolves
# at run time. npm ci fails if a lock disagrees with the package.json at the pinned
# commit, so a PINNED bump that is not followed by a lock regeneration is caught.
# Regenerate: see README.md. The packages depend on each other through file: links,
# so the client is installed and built first, then the agent service.
ts_client=$src/client-sdk/typescript
ts_agent=$src/agent-sdk/typescript
cp "$here/locks/ts-client/package-lock.json" "$ts_client/package-lock.json"
cp "$here/locks/ts-agent/package-lock.json" "$ts_agent/package-lock.json"
(cd "$ts_client" && npm ci --no-audit --no-fund --ignore-scripts && npm run build)
(cd "$ts_agent" && npm ci --no-audit --no-fund --ignore-scripts && npm run build)
if [ ! -f "$ts_client/dist/index.js" ] || [ ! -f "$ts_agent/dist/index.js" ]; then
	echo "setup.sh: the TypeScript build produced no dist" >&2
	exit 1
fi

# 3. Python SDKs in a venv inside the cache. The external dependencies come from the
# hashed lock locks/py/requirements.txt (--require-hashes: every wheel or sdist is
# checked against it, and nothing outside it can be installed). The two upstream
# packages are local source trees and cannot carry a hash; they are installed
# editable with --no-deps after the hashed set, and --no-build-isolation makes the
# build use the hashed hatchling instead of resolving one, so nothing resolves.
uv venv --quiet --allow-existing --python "$pyver" "$venv"
uv pip install --quiet --python "$venv/bin/python" --require-hashes --no-deps -r "$here/locks/py/requirements.txt"
uv pip install --quiet --python "$venv/bin/python" --no-deps --no-build-isolation -e "$src/client-sdk/python" -e "$src/agent-sdk/python"
uv pip check --python "$venv/bin/python"

# 4. Record exact versions.
{
	echo "upstream_sha=$sha"
	echo "node=$(node --version)"
	echo "npm=$(npm --version)"
	echo "python=$("$venv/bin/python" --version 2>&1)"
	echo "uv=$(uv --version)"
	node -e '
const pkg = (d) => require(d + "/package.json");
const dep = (d, n) => require(d + "/node_modules/" + n + "/package.json").version;
const c = process.argv[1], a = process.argv[2];
console.log("ts_client=" + pkg(c).name + "@" + pkg(c).version);
console.log("ts_agent=" + pkg(a).name + "@" + pkg(a).version);
for (const n of ["@nats-io/nats-core", "@nats-io/transport-node", "@nats-io/services", "@nats-io/nkeys"]) console.log("ts_dep=" + n + "@" + dep(c, n));
' "$ts_client" "$ts_agent"
	"$venv/bin/python" -I -c '
from importlib.metadata import version
for n in ("synadia-ai-agents", "synadia-ai-agent-service", "nats-py", "pydantic", "nkeys", "pynacl"):
    print("py_pkg=%s==%s" % (n, version(n)))
'
} >"$root/versions.txt"

date -u +%Y-%m-%dT%H:%M:%SZ >"$root/ready"
echo "interop cache built: $root"
cat "$root/versions.txt"
print_env
