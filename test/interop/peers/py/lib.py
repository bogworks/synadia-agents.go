# Copyright 2026 The synadia-agents.go Authors
# SPDX-License-Identifier: Apache-2.0
"""Shared plumbing for the Python interop peers.

Environment, JSON-line output, and a connection opened through the official
SDK's connection bundle. The peers import the OFFICIAL SDKs from the venv that
setup.sh builds; nothing here reimplements the protocol.
"""

from __future__ import annotations

import asyncio
import base64
import hashlib
import json
import os
import signal
import sys
from typing import Any

import nats
from synadia_ai.agents import resolve_nats_connection_bundle


def env(name: str, default: str | None = None) -> str | None:
    value = os.environ.get(name)
    return default if value is None or value == "" else value


def emit(obj: dict[str, Any]) -> None:
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


class Connection:
    """One NATS connection plus the bundle that owns its signing snapshot."""

    def __init__(self, nc: Any, bundle: Any) -> None:
        self.nc = nc
        self.bundle = bundle

    @property
    def signer(self) -> Any:
        return self.bundle.signer

    async def close(self) -> None:
        if not self.nc.is_closed:
            await self.nc.close()
        self.bundle.wipe()


async def connect() -> Connection:
    """SAP_SEED_FILE authenticates as that nkey user; SAP_SIGN=1 also signs sender identity."""
    url = env("SAP_NATS_URL", "nats://127.0.0.1:4222")
    seed = env("SAP_SEED_FILE")
    sign = env("SAP_SIGN") == "1"
    if sign and not seed:
        raise SystemExit("SAP_SIGN=1 needs SAP_SEED_FILE")
    bundle = resolve_nats_connection_bundle(url=url, nkey=seed, identity="signed" if sign else "off")
    try:
        nc = await nats.connect(**bundle.connection_options)
    except BaseException:
        bundle.wipe()
        raise
    return Connection(nc, bundle)


async def until_signalled() -> None:
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)
    await stop.wait()


def describe_attachments(items: Any) -> list[dict[str, Any]]:
    """Name, decoded size and SHA-256 (hex) of each attachment; contents never leave the peer."""
    out = []
    for a in items or []:
        raw = base64.b64decode(a.content)
        out.append({"filename": a.filename, "size": len(raw), "sha256": hashlib.sha256(raw).hexdigest()})
    return out
