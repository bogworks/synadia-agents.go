# Copyright 2026 The synadia-agents.go Authors
# SPDX-License-Identifier: Apache-2.0
"""Interop host: an SAP agent on the official Python AgentService.

Environment (same names and meanings as the TypeScript host, peers/ts/host.mjs):
  SAP_NATS_URL, SAP_SEED_FILE, SAP_SIGN   connection
  SAP_AGENT, SAP_OWNER, SAP_NAME          the three identity tokens (default interop/interop/main)
  SAP_MODE      echo | query | error | attachments | slow | signed   (default echo)
  SAP_CHUNKS    echo: number of response chunks (default 3)
  SAP_ERROR_CODE, SAP_ERROR_DESC          error mode: the rejection (default 503 "interop error")
  SAP_SLOW_MS, SAP_KEEPALIVE_S            slow mode: handler delay and ack cadence (default 1500, 0.2)
  SAP_HEARTBEAT_S                         heartbeat interval (default 1)
  SAP_MIN_SENDER_TRUST                    any | signed (signed mode defaults to signed)

Output: JSON lines on stdout: {"type":"ready",...}, then one {"type":"prompt",...}
per request, {"type":"query_reply",...} in query mode.
"""

from __future__ import annotations

import asyncio
import math

from lib import connect, describe_attachments, emit, env, until_signalled
from synadia_ai.agent_service import AgentService, RequestRejectedError, ServiceIdentity
from synadia_ai.agents import Attachment, Envelope, ResponseChunk, format_sender

MODE = env("SAP_MODE", "echo")
CHUNKS = int(env("SAP_CHUNKS", "3"))
ERROR_CODE = int(env("SAP_ERROR_CODE", "503"))
ERROR_DESC = env("SAP_ERROR_DESC", "interop error")
SLOW_S = int(env("SAP_SLOW_MS", "1500")) / 1000
SIGN = MODE == "signed" or env("SAP_SIGN") == "1"
MIN_TRUST = env("SAP_MIN_SENDER_TRUST", "signed" if MODE == "signed" else "any")


def split(text: str, n: int) -> list[str]:
    size = max(1, math.ceil(len(text) / n))
    return [text[i : i + size] for i in range(0, len(text), size)] or [""]


async def main() -> None:
    conn = await connect()
    service = AgentService(
        nc=conn.nc,
        agent=env("SAP_AGENT", "interop"),
        owner=env("SAP_OWNER", "interop"),
        session_name=env("SAP_NAME", "main"),
        description=f"interop {MODE} host (python)",
        heartbeat_interval_s=float(env("SAP_HEARTBEAT_S", "1")),
        keepalive_interval_s=float(env("SAP_KEEPALIVE_S", "0.2")) if MODE == "slow" else None,
        min_sender_trust=MIN_TRUST,
        identity=ServiceIdentity(signer=conn.signer) if SIGN and conn.signer else None,
    )

    async def handler(envelope: Envelope, stream) -> None:
        emit(
            {
                "type": "prompt",
                "prompt": envelope.prompt,
                "attachments": describe_attachments(envelope.attachments),
                "sender": format_sender(stream.sender) if stream.sender is not None else None,
            }
        )
        if MODE == "error":
            raise RequestRejectedError(ERROR_CODE, ERROR_DESC)
        if MODE == "query":
            await stream.send("thinking")
            answer = await stream.ask("proceed?", timeout=10.0)
            emit({"type": "query_reply", "prompt": answer.prompt})
            await stream.send(f"reply: {answer.prompt}")
        elif MODE == "attachments":
            echoed = [Attachment(filename=a.filename, content=a.content) for a in envelope.attachments or []]
            await stream.send(ResponseChunk(text=f"received {len(echoed)}", attachments=echoed or None))
        else:
            if MODE == "slow":
                await asyncio.sleep(SLOW_S)
            for part in split(envelope.prompt, CHUNKS):
                await stream.send(part)

    service.on_prompt(handler)
    await service.start()
    emit({"type": "ready", "impl": "python", "subject": service.subject.prompt, "mode": MODE, "min_sender_trust": MIN_TRUST})
    await until_signalled()
    await service.stop()
    await conn.close()


asyncio.run(main())
