import { randomUUID } from "node:crypto";

import type { Command, CommandCreateRequest, Session } from "../generated/types.ts";
import type { Palai } from "../client.ts";
import { callArgs, enc, listPath, type CallOptions, type ListParams, type Page } from "./shared.ts";

// SteerParams / InterruptParams carry the message a steer/interrupt delivers. command_id is the
// command's own idempotency handle (spec §22.4): the SDK mints a stable one per call and lets a
// caller supply their own to dedupe across processes — mirroring the idempotency key in
// responses.create.
export interface SteerParams {
  message: string;
  commandId?: string;
}
export type InterruptParams = SteerParams;

// SessionCommands is the durable command surface of a session (spec §9.2, §22.4) — E08's steering
// product, used from the SDK for the first time. steer and interrupt are both send_message
// deliveries; the delivery mode is the only difference (queue is the default fire path and not
// exposed here — the plan asks for steer + interrupt).
export class SessionCommands {
  #client: Palai;
  constructor(client: Palai) {
    this.#client = client;
  }

  // steer delivers a message at the next safe boundary WITHOUT interrupting the current step
  // (delivery=steer). Acceptance is durable-queued (202), not applied; the returned Command is the
  // queued handle, or — on a duplicate command_id — the original unchanged (idempotent).
  steer(sessionID: string, params: SteerParams, options: CallOptions = {}): Promise<Command> {
    return this.#send(sessionID, "steer", params, options);
  }

  // interrupt delivers a message that preempts the current step (delivery=interrupt). Same durable,
  // idempotent acceptance as steer.
  interrupt(sessionID: string, params: InterruptParams, options: CallOptions = {}): Promise<Command> {
    return this.#send(sessionID, "interrupt", params, options);
  }

  async #send(sessionID: string, delivery: "steer" | "interrupt", params: SteerParams, options: CallOptions): Promise<Command> {
    const body: CommandCreateRequest = {
      command_id: params.commandId ?? newCommandId(),
      kind: "send_message",
      delivery,
      message: params.message,
    };
    const result = await this.#client.request<Command>("POST", `/v1/sessions/${enc(sessionID)}/commands`, {
      body,
      ...callArgs(options),
    });
    return result.body;
  }
}

// Sessions is the /v1/sessions resource: create, retrieve, list, and the durable command surface.
export class Sessions {
  #client: Palai;
  readonly commands: SessionCommands;

  constructor(client: Palai) {
    this.#client = client;
    this.commands = new SessionCommands(client);
  }

  // create opens a standalone session (201). Creation is cheap and unkeyed — a retried create mints
  // a new session — so it carries no idempotency key.
  async create(options: CallOptions = {}): Promise<Session> {
    const result = await this.#client.request<Session>("POST", "/v1/sessions", callArgs(options));
    return result.body;
  }

  // retrieve reads a session within the verified scope; an unknown or foreign id is a 404
  // (NotFoundError).
  async retrieve(sessionID: string, options: CallOptions = {}): Promise<Session> {
    const result = await this.#client.request<Session>("GET", `/v1/sessions/${enc(sessionID)}`, callArgs(options));
    return result.body;
  }

  // list returns a tenant-scoped page of sessions with the shared cursor + basic filters.
  async list(params: ListParams = {}, options: CallOptions = {}): Promise<Page<Session>> {
    const result = await this.#client.request<Page<Session>>("GET", listPath("/v1/sessions", params), callArgs(options));
    return result.body;
  }
}

function newCommandId(): string {
  return `cmd_${randomUUID()}`;
}
