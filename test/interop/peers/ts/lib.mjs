// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0
//
// Shared plumbing for the TypeScript interop peers: environment, JSON-line
// output, and a connection opened through the official SDK's connection bundle.
// The peers import the OFFICIAL SDKs from the pinned checkout named by
// SAP_INTEROP_SRC (see setup.sh); nothing here reimplements the protocol.

import { createHash } from "node:crypto";
import { createRequire } from "node:module";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const src = process.env.SAP_INTEROP_SRC;
if (!src) {
  process.stderr.write("SAP_INTEROP_SRC is not set (run test/interop/setup.sh)\n");
  process.exit(64);
}

const clientDir = join(src, "client-sdk", "typescript");
const agentDir = join(src, "agent-sdk", "typescript");

export const client = await import(pathToFileURL(join(clientDir, "dist", "index.js")).href);
export const agentSdk = await import(pathToFileURL(join(agentDir, "dist", "index.js")).href);
const transportPath = createRequire(join(clientDir, "package.json")).resolve("@nats-io/transport-node");
const transport = await import(pathToFileURL(transportPath).href);

/** Env var with an optional default. */
export const env = (name, fallback) => {
  const v = process.env[name];
  return v === undefined || v === "" ? fallback : v;
};

/** Write one JSON line to stdout. */
export const emit = (obj) => process.stdout.write(`${JSON.stringify(obj)}\n`);

/**
 * Open a NATS connection through the SDK's bundle. SAP_SEED_FILE authenticates
 * as that nkey user; SAP_SIGN=1 additionally signs sender identity (SAP §13).
 * Returns { nc, signer, close }.
 */
export async function connect() {
  const url = env("SAP_NATS_URL", "nats://127.0.0.1:4222");
  const seed = env("SAP_SEED_FILE");
  const sign = env("SAP_SIGN") === "1";
  if (sign && !seed) throw new Error("SAP_SIGN=1 needs SAP_SEED_FILE");
  const bundle = await client.resolveNatsConnectionBundle(seed ? { url, nkey: seed } : { url }, {
    identity: sign ? "signed" : "off",
  });
  const nc = await transport.connect(bundle.connectionOptions);
  return {
    nc,
    signer: bundle.signer,
    async close() {
      await nc.close();
      bundle.wipe();
    },
  };
}

/** Resolve when SIGINT or SIGTERM arrives. */
export const untilSignalled = () =>
  new Promise((resolve) => {
    process.once("SIGINT", resolve);
    process.once("SIGTERM", resolve);
  });

/**
 * Describe attachments by name, decoded size and SHA-256 (hex). Request
 * attachments carry bytes; response and query attachments carry base64 text.
 */
export const describeAttachments = (list) =>
  (list ?? []).map((a) => {
    const bytes = typeof a.content === "string" ? Buffer.from(a.content, "base64") : Buffer.from(a.content);
    return { filename: a.filename, size: bytes.length, sha256: createHash("sha256").update(bytes).digest("hex") };
  });
