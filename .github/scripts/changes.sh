#!/usr/bin/env bash
# Copyright 2026 The synadia-agents.go Authors
# SPDX-License-Identifier: Apache-2.0
#
# Decides whether a change needs the full CI run. Reads the changed file names (one per line) on stdin and prints
# `code=true` or `code=false`; the changes jobs of go-sdk.yml and interop.yml append that line to $GITHUB_OUTPUT.
#
# Work is skipped, checks are not: the heavy jobs wait on this answer, and pr-check/interop-check still run and
# pass only when everything that had to run did. false means docs-only: every changed file is documentation.
#
# code=true (always the safe answer) when:
#   - the event is not a pull_request (push to main, merge_group, workflow_dispatch): everything runs in full;
#   - the pull request is a Mergify merge-queue draft (head ref mergify/merge-queue/*, author mergify[bot], same
#     repository): the queue's own run always runs in full;
#   - no file is listed (an empty diff proves nothing);
#   - any changed file is not documentation. Documentation is the narrow set below; everything else is code, so a
#     new kind of file defaults to a full run.
#
# Documentation (checked after the code-wins list): any *.md, anything under docs/, LICENSE, CODE_OF_CONDUCT and
# .gitignore. Code wins over that: anything under .github/ (scripts, templates and workflows are the CI itself) and any
# testdata/ or test-fixtures/ directory, which tests read whatever the extension. The tests that read documentation (test/docs,
# the ADR lint of docs/decisions, the README quick start check) and the independence grep are cheap and still run on a
# docs-only change; see the docs-guard and independence jobs of go-sdk.yml. test/ci fails if a test reads a Markdown
# file that docs-guard does not run.
#
# Environment: EVENT_NAME, HEAD_REF, PR_AUTHOR, HEAD_REPO, REPO.
set -euo pipefail

if [ "${EVENT_NAME:-}" != pull_request ]; then
  echo "code=true"
  exit 0
fi
case "${HEAD_REF:-}" in
  mergify/merge-queue/*)
    if [ "${PR_AUTHOR:-}" = 'mergify[bot]' ] && [ -n "${REPO:-}" ] && [ "${HEAD_REPO:-}" = "$REPO" ]; then
      echo "code=true"
      exit 0
    fi
    ;;
esac

count=0
code=false
while IFS= read -r f; do
  [ -n "$f" ] || continue
  count=$((count + 1))
  case "$f" in
    .github/* | testdata/* | */testdata/* | test-fixtures/* | */test-fixtures/*) code=true ;;
    *.go | go.mod | go.sum | */go.mod | */go.sum) code=true ;;
    *.md | docs/* | LICENSE | CODE_OF_CONDUCT | .gitignore) ;;
    *) code=true ;;
  esac
done
[ "$count" -gt 0 ] || code=true
echo "code=$code"
