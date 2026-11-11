# Copyright 2026 The synadia-agents.go Authors
# SPDX-License-Identifier: Apache-2.0
"""Interop caller: discovers an SAP agent and prompts it through the official
Python client, printing everything it receives as JSON lines.

Environment (same names and meanings as the TypeScript caller, peers/ts/caller.mjs):
  SAP_NATS_URL, SAP_SEED_FILE, SAP_SIGN   connection; SAP_SIGN=1 signs the prompt
  SAP_AGENT, SAP_OWNER, SAP_NAME          discovery filter (any may be empty)
  SAP_PROMPT                              prompt text; unset means discover (and watch/status) only
  SAP_ATTACHMENTS_JSON                    [{"filename":"a.txt","content_b64":"..."}]
  SAP_QUERY_ANSWER                        answer to any mid-stream query (default "yes")
  SAP_STATUS=1                            also call the status endpoint
  SAP_WATCH_HEARTBEAT_S                   observe heartbeats this many seconds before prompting
  SAP_TIMEOUT_S                           inactivity timeout and ceiling (default 10)
  SAP_DEFAULT_TIMEOUTS=1                  omit both timeout options so the SDK defaults apply

Output lines: identity, discovered, status_reply, heartbeat, liveness, response, status,
query, error, done. Exit 0 when the stream ended (done or error), 2 when no agent
matched, 1 on a harness failure.
"""

from __future__ import annotations

import asyncio
import base64
import json
import re
import sys
import traceback

from lib import connect, describe_attachments, emit, env
from synadia_ai.agents import (
    Agents,
    Attachment,
    DiscoverFilter,
    Identity,
    Query,
    ResponseChunk,
    StatusChunk,
)


async def main() -> int:
    timeout = float(env("SAP_TIMEOUT_S", "10"))
    conn = await connect()
    agents = Agents(nc=conn.nc, identity=Identity(signer=conn.signer) if conn.signer else None)
    code = 0
    try:
        if conn.signer:
            emit({"type": "identity", "id": str(await agents.self_id())})
        await agents.start_tracking()
        found = await agents.discover(
            filter=DiscoverFilter(agent=env("SAP_AGENT"), owner=env("SAP_OWNER"), session_name=env("SAP_NAME"))
        )
        emit(
            {
                "type": "discovered",
                "agents": [
                    {
                        "agent": a.agent,
                        "owner": a.owner,
                        "name": a.session_name,
                        "subject": a.prompt_subject,
                        "protocol_version": a.protocol_version,
                        "min_sender_trust": a.prompt_endpoint.min_sender_trust,
                        "attachments_ok": a.prompt_endpoint.attachments_ok,
                        "max_payload": a.prompt_endpoint.max_payload_bytes,
                    }
                    for a in found
                ],
            }
        )
        if not found:
            return 2
        agent = found[0]

        watch = float(env("SAP_WATCH_HEARTBEAT_S", "0"))
        if watch > 0:
            off = agents.on_heartbeat(
                agent.instance_id,
                lambda hb: emit({"type": "heartbeat", "agent": hb.agent, "owner": hb.owner, "interval_s": hb.interval_s, "ts": hb.ts}),
            )
            await asyncio.sleep(watch)
            off()
            live = agents.liveness(agent.instance_id)
            emit({"type": "liveness", "online": live.is_online if live else None})
        if env("SAP_STATUS") == "1":
            hb = await agent.status()
            emit({"type": "status_reply", "agent": hb.agent, "owner": hb.owner, "interval_s": hb.interval_s})

        prompt = env("SAP_PROMPT")
        if prompt is not None:
            attachments = [
                Attachment.from_bytes(a["filename"], base64.b64decode(a["content_b64"]))
                for a in json.loads(env("SAP_ATTACHMENTS_JSON", "[]"))
            ]
            n = 0
            try:
                kw = {} if env("SAP_DEFAULT_TIMEOUTS") == "1" else {"timeout": timeout, "max_wait_s": timeout}
                async for msg in agent.prompt(prompt, attachments=attachments or None, **kw):
                    n += 1
                    if isinstance(msg, ResponseChunk):
                        emit({"type": "response", "text": msg.text, "attachments": describe_attachments(msg.attachments)})
                    elif isinstance(msg, StatusChunk):
                        emit({"type": "status", "status": msg.status})
                    elif isinstance(msg, Query):
                        emit({"type": "query", "id": msg.id, "prompt": msg.prompt, "attachments": describe_attachments(msg.attachments)})
                        await msg.reply(env("SAP_QUERY_ANSWER", "yes"))
                emit({"type": "done", "chunks": n})
            except Exception as exc:  # noqa: BLE001 - every stream failure is reported, not raised
                m = re.match(r"service error (\d+)", str(exc))
                emit({"type": "error", "name": type(exc).__name__, "code": int(m.group(1)) if m else None, "message": str(exc)})
    except Exception:
        traceback.print_exc()
        code = 1
    finally:
        await agents.close()
        await conn.close()
    return code


sys.exit(asyncio.run(main()))
