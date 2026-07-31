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

// SessionCreateParams carries the one thing a caller may supply when opening a session: its display
// label. Omitting it is the pre-existing bodyless create, unchanged.
export interface SessionCreateParams {
  name?: string;
}

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
      // command_id carries idempotency server-side, so a network re-send settles exactly one command.
      idempotent: true,
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
  // a new session — so it carries no idempotency key. `name` is the optional display label; without
  // one the session's projection derives a label from its first prompt (see `rename`).
  async create(params: SessionCreateParams = {}, options: CallOptions = {}): Promise<Session> {
    const args = callArgs(options);
    const result = await this.#client.request<Session>(
      "POST",
      "/v1/sessions",
      params.name === undefined ? args : { body: { name: params.name }, ...args },
    );
    return result.body;
  }

  // rename sets a session's display label and returns the re-read session, so the caller sees both
  // the label and the `name_source` that now reads "operator".
  //
  // This, and not the create-time name, is the call a session list needs: most sessions are never
  // created through `create` at all — posting a response opens one implicitly — so they reach a list
  // screen with no label to have been given one. Passing an EMPTY string is meaningful: it clears the
  // operator label and returns the row to its derived one.
  async rename(sessionID: string, name: string, options: CallOptions = {}): Promise<Session> {
    const result = await this.#client.request<Session>("PATCH", `/v1/sessions/${enc(sessionID)}`, {
      body: { name },
      // Setting a label to X twice leaves it X, so a connection torn after the server committed is
      // safe to re-send. That is a property of THIS call, not of PATCH, which is why it is declared
      // here rather than in the transport's method table.
      idempotent: true,
      ...callArgs(options),
    });
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
