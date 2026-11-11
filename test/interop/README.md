# Interop harness (AT-003)

This directory runs the Go SDK against the official Synadia Agents SDKs
(TypeScript and Python) over a real `nats-server`. A Go host is driven by official
callers, and a Go caller is pointed at official hosts, so each side of SAP 0.3 is
checked against two independent implementations.

## What it covers

- `gohost_test.go`, `gohost_keepalive_test.go`, `gohost_helpers_test.go`: a Go
  host served to the official TypeScript and Python callers (discovery, prompt and
  streamed response, mid-stream query, error, attachments, status, heartbeat,
  keep-alive, signed sender identity).
- `gocaller_test.go`: a Go caller against official TypeScript and Python hosts in
  the same scenarios.
- `selftest_test.go`: checks on the harness itself.
- `interop.go`: the harness (embedded server, peer processes, JSON-line reports).
- `peers/ts`, `peers/py`: thin drivers over the official SDKs, one host and one
  caller per language. They print what they saw as JSON lines on stdout.

## Prerequisites

- Go (the version in `test/go.mod`).
- `git`, `node` (20 or newer), `npm` and `uv` on `PATH`. `uv` fetches the Python
  named in `PINNED` when it is missing. The interop CI job installs node 22 and
  uv with SHA-pinned `actions/setup-node` and `astral-sh/setup-uv` (caching off).
- Network access to GitHub, the npm registry and PyPI on the first run only. A
  finished cache needs none.

## Run locally

```sh
test/interop/setup.sh                 # fetch and build the official SDKs, once
cd test
go test -count=1 ./interop/...
```

`setup.sh` writes only under `~/.cache/synadia-agents-interop/<sha>/`, never into
the repository, and is idempotent. Without the cache the interop tests skip, so
`go test ./...` stays green on a machine without node or Python.

## Environment variables

| Variable | Read by | Meaning |
| --- | --- | --- |
| `SAP_INTEROP_REQUIRED=1` | `interop.go` | A missing cache fails the tests instead of skipping them. CI sets it (`.github/workflows/interop.yml`), because AT-003 treats a skipped scenario as a failure. |
| `SAP_INTEROP_CACHE` | `interop.go`, `setup.sh` | Cache root. Default `$HOME/.cache/synadia-agents-interop`. |
| `SAP_INTEROP_SOURCE` | `setup.sh` | Git URL or path to fetch the upstream source from instead of `UPSTREAM_REPO`. |
| `SAP_INTEROP_FORCE=1` | `setup.sh` | Rebuild even if the cache is marked ready. |
| `SAP_INTEROP_LONG=1` | `gohost_keepalive_test.go` | Run the 70 second keep-alive experiment, which is skipped by default. |
| `SAP_INTEROP_EVIDENCE=<dir>` | `interop.go` | Write each peer's JSON lines to `<dir>/<test>/<peer>.jsonl`, the transcript AT-003 asks a run to keep. |
| `SAP_DEFAULT_TIMEOUTS=1` | `peers/*/caller.*` | Set by a test through `Peer.Extra`, not by hand: the caller peer omits its timeout options so the official SDK defaults apply. |

The harness removes every other `SAP_*` variable from the environment it passes to
a peer and sets the peer's own, so a stray variable in your shell cannot change a
scenario.

## Pinning and locks

`PINNED` names the upstream commit. Nothing resolves at run time:

- `locks/ts-client/package-lock.json` and `locks/ts-agent/package-lock.json` are npm
  lockfiles for `client-sdk/typescript` and `agent-sdk/typescript` (upstream ships
  bun lockfiles, which npm does not read). `setup.sh` copies each in and runs
  `npm ci --ignore-scripts`, which fails if a lock disagrees with the package's
  `package.json`.
- `locks/py/requirements.txt` is a hashed lock (every wheel and sdist) of the Python
  dependencies and the `hatchling` build backend. `setup.sh` installs it with
  `--require-hashes --no-deps`, then installs the two local upstream packages
  editable with `--no-deps --no-build-isolation`. Those two are source trees and
  cannot be hashed; they pull in nothing.

## Regenerating the locks

Do this whenever `UPSTREAM_SHA` in `PINNED` changes, or to take newer dependency
versions. Use the upstream tree at the new commit (the cache's `src/` after a
`setup.sh` run, or a fresh checkout), and delete any `package-lock.json` already in
those directories first.

```sh
src=~/.cache/synadia-agents-interop/<sha>/src
for p in client-sdk:ts-client agent-sdk:ts-agent; do
  rm -f "$src/${p%%:*}/typescript/package-lock.json"
  (cd "$src/${p%%:*}/typescript" && npm install --package-lock-only --ignore-scripts)
  cp "$src/${p%%:*}/typescript/package-lock.json" test/interop/locks/${p##*:}/
done

# Python: update locks/py/requirements.in if either package's dependencies changed
# (see its pyproject.toml), then compile with relative paths, so no local path
# lands in the lock, and put the four-line header back:
cd test/interop/locks/py
head -4 requirements.txt > header.txt
uv pip compile requirements.in --python-version 3.13 --universal \
  --generate-hashes --no-header -o requirements.new
cat header.txt requirements.new > requirements.txt && rm header.txt requirements.new
```

Keep `--python-version` equal to `PYTHON_VERSION` in `PINNED`, then run `SAP_INTEROP_FORCE=1 test/interop/setup.sh` and the interop tests, and commit the
locks together with the `PINNED` change.
