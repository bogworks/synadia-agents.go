// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0
//
// Interop caller: discovers an SAP agent and prompts it through the official
// TypeScript client, printing everything it receives as JSON lines.
//
// Environment:
//   SAP_NATS_URL, SAP_SEED_FILE, SAP_SIGN   connection (see lib.mjs); SAP_SIGN=1 signs the prompt
//   SAP_AGENT, SAP_OWNER, SAP_NAME          discovery filter (any may be empty)
//   SAP_PROMPT                              prompt text; unset means discover (and watch/status) only
//   SAP_ATTACHMENTS_JSON                    [{"filename":"a.txt","content_b64":"..."}]
//   SAP_QUERY_ANSWER                        answer to any mid-stream query (default "yes")
//   SAP_STATUS=1                            also call the status endpoint
//   SAP_WATCH_HEARTBEAT_S                   observe heartbeats this many seconds before prompting
//   SAP_TIMEOUT_S                           inactivity timeout and ceiling (default 10)
//   SAP_DEFAULT_TIMEOUTS=1                  omit both timeout options so the SDK defaults apply
//
// Output lines: identity, discovered, status_reply, heartbeat, response, status, query,
// error, done. Exit 0 when the stream ended (done or error), 2 when no agent matched,
// 1 on a harness failure.

import { client, connect, describeAttachments, emit, env } from "./lib.mjs";

const timeoutMs = Number(env("SAP_TIMEOUT_S", "10")) * 1000;
const filter = Object.fromEntries(
  [["agent", "SAP_AGENT"], ["owner", "SAP_OWNER"], ["name", "SAP_NAME"]]
    .map(([k, v]) => [k, env(v)])
    .filter(([, v]) => v !== undefined),
);

const conn = await connect();
const agents = new client.Agents({ nc: conn.nc, ...(conn.signer ? { identity: { signer: conn.signer } } : {}) });
let code = 0;
try {
  if (conn.signer) emit({ type: "identity", id: await agents.selfId() });
  await agents.startTracking();
  const found = await agents.discover({ filter });
  emit({
    type: "discovered",
    agents: found.map((a) => ({
      agent: a.agent,
      owner: a.owner,
      name: a.name,
      subject: a.promptEndpoint.subject,
      protocol_version: a.protocolVersion,
      min_sender_trust: a.promptEndpoint.minSenderTrust ?? null,
      attachments_ok: a.promptEndpoint.attachmentsOk ?? null,
      max_payload: a.promptEndpoint.maxPayloadBytes ?? null,
    })),
  });
  const agent = found[0];
  if (!agent) {
    code = 2;
  } else {
    const watch = Number(env("SAP_WATCH_HEARTBEAT_S", "0"));
    if (watch > 0) {
      const off = agents.onHeartbeat(agent.instanceId, (hb) =>
        emit({ type: "heartbeat", agent: hb.agent, owner: hb.owner, interval_s: hb.intervalS, ts: hb.ts }),
      );
      await new Promise((r) => setTimeout(r, watch * 1000));
      off();
      emit({ type: "liveness", online: agents.liveness(agent.instanceId)?.isOnline ?? null });
    }
    if (env("SAP_STATUS") === "1") {
      const hb = await agent.status();
      emit({ type: "status_reply", agent: hb.agent, owner: hb.owner, interval_s: hb.intervalS });
    }
    const prompt = env("SAP_PROMPT");
    if (prompt !== undefined) {
      const attachments = JSON.parse(env("SAP_ATTACHMENTS_JSON", "[]")).map((a) => ({
        filename: a.filename,
        content: new Uint8Array(Buffer.from(a.content_b64, "base64")),
      }));
      let n = 0;
      try {
        const stream = await agent.prompt(prompt, {
          attachments,
          ...(env("SAP_DEFAULT_TIMEOUTS") === "1" ? {} : { maxWaitMs: timeoutMs, inactivityTimeoutMs: timeoutMs }),
        });
        for await (const msg of stream) {
          n += 1;
          if (msg.type === "response") {
            emit({ type: "response", text: msg.text, attachments: describeAttachments(msg.attachments) });
          } else if (msg.type === "status") {
            emit({ type: "status", status: msg.status });
          } else {
            emit({ type: "query", id: msg.id, prompt: msg.prompt, attachments: describeAttachments(msg.attachments) });
            await msg.reply(env("SAP_QUERY_ANSWER", "yes"));
          }
        }
        emit({ type: "done", chunks: n });
      } catch (err) {
        emit({ type: "error", name: err.name, code: err.code ?? null, message: err.message });
      }
    }
  }
} catch (err) {
  process.stderr.write(`${err?.stack ?? err}\n`);
  code = 1;
} finally {
  await agents.close();
  await conn.close();
}
process.exit(code);
