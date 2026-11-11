# Source of the fixtures

`agent-tools/` is a verbatim copy of `test-fixtures/agent-tools/` of the
official Synadia agents repository (the language-neutral AgentTools
vectors), at commit `a1bfb47`. The upstream `README.md` is included.

Only the files in `agent-tools/` come from upstream; nothing in them was
edited. `../definitions/*.json` are the copies the package embeds (as the
official TypeScript and Python SDKs embed their own); a test requires every
one to be byte-identical to its fixture here.

To update: copy the new upstream files into both directories, change this
commit id, and run `go test ./tools/...`.
