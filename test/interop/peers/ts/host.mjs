// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0
//
// Interop host: an SAP agent on the official TypeScript AgentService.
//
// Environment:
//   SAP_NATS_URL, SAP_SEED_FILE, SAP_SIGN   connection (see lib.mjs)
//   SAP_AGENT, SAP_OWNER, SAP_NAME          the three identity tokens (default interop/interop/main)
//   SAP_MODE      echo | query | error | attachments | slow | signed   (default echo)
//   SAP_CHUNKS    echo: number of response chunks (default 3)
//   SAP_ERROR_CODE, SAP_ERROR_DESC          error mode: the rejection (default 503 "interop error")
//   SAP_SLOW_MS, SAP_KEEPALIVE_S            slow mode: handler delay and ack cadence (default 1500, 0.2)
//   SAP_HEARTBEAT_S                         heartbeat interval (default 1)
//   SAP_MIN_SENDER_TRUST                    any | signed (signed mode defaults to signed)
//
// Output: JSON lines on stdout: {"type":"ready",...}, then one {"type":"prompt",...}
// per request, {"type":"query_reply",...} in query mode.

import { agentSdk, client, connect, describeAttachments, emit, env, untilSignalled } from "./lib.mjs";

const mode = env("SAP_MODE", "echo");
const chunks = Number(env("SAP_CHUNKS", "3"));
const errorCode = Number(env("SAP_ERROR_CODE", "503"));
const errorDesc = env("SAP_ERROR_DESC", "interop error");
const slowMs = Number(env("SAP_SLOW_MS", "1500"));
const sign = mode === "signed" || env("SAP_SIGN") === "1";
if (mode === "signed") process.env.SAP_SIGN = "1";
const minTrust = env("SAP_MIN_SENDER_TRUST", mode === "signed" ? "signed" : "any");

const split = (text, n) => {
  const runes = Array.from(text);
  const size = Math.max(1, Math.ceil(runes.length / n));
  const parts = [];
  for (let i = 0; i < runes.length; i += size) parts.push(runes.slice(i, i + size).join(""));
  return parts.length ? parts : [""];
};

const conn = await connect();
const service = new agentSdk.AgentService({
  nc: conn.nc,
  agent: env("SAP_AGENT", "interop"),
  owner: env("SAP_OWNER", "interop"),
  name: env("SAP_NAME", "main"),
  description: `interop ${mode} host (typescript)`,
  heartbeatIntervalS: Number(env("SAP_HEARTBEAT_S", "1")),
  keepaliveIntervalS: mode === "slow" ? Number(env("SAP_KEEPALIVE_S", "0.2")) : null,
  minSenderTrust: minTrust,
  ...(sign && conn.signer ? { identity: { signer: conn.signer } } : {}),
});

service.onPrompt(async (envelope, response) => {
  emit({
    type: "prompt",
    prompt: envelope.prompt,
    attachments: describeAttachments(envelope.attachments),
    sender: response.sender ? client.formatSender(response.sender) : null,
  });
  switch (mode) {
    case "error":
      throw new agentSdk.RequestRejectedError(errorCode, errorDesc);
    case "query": {
      await response.send("thinking");
      const answer = await response.ask("proceed?", { timeoutMs: 10_000 });
      emit({ type: "query_reply", prompt: answer.prompt });
      await response.send(`reply: ${answer.prompt}`);
      break;
    }
    case "attachments":
      await response.send({
        type: "response",
        text: `received ${envelope.attachments?.length ?? 0}`,
        attachments: (envelope.attachments ?? []).map((a) => ({
          filename: a.filename,
          content: Buffer.from(a.content).toString("base64"),
        })),
      });
      break;
    case "slow":
      await new Promise((r) => setTimeout(r, slowMs));
    // falls through
    default:
      for (const part of split(envelope.prompt, chunks)) await response.send(part);
  }
});

await service.start();
emit({ type: "ready", impl: "typescript", subject: service.subject.prompt, mode, min_sender_trust: minTrust });
await untilSignalled();
await service.stop();
await conn.close();
