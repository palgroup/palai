"""The resource groups — a one-to-one mirror of the TS SDK's ``resources/*.ts`` public surface.

Every method is a thin pass-through to ``client.request(...)``, which returns the decoded body. On
the SYNC client that body is a value; on the ASYNC client the same call returns an awaitable of that
value — so ONE resource implementation serves both clients (the caller ``await``s on the async one).
The only client-type-specific pieces (``responses.stream`` and ``artifacts.download``) defer to a
factory the client supplies, so those too need no sync/async fork here.

Returns are annotated ``Any`` deliberately: a value under the sync client, an ``Awaitable`` of the
same shape under the async client. The runtime value is always a plain ``dict`` (or list envelope),
so unknown server fields survive a round-trip (the open-world stance — API-009). Generated typed
views are NOT hand-copied here: the "generated types are the single source" invariant (plan §2)
forbids a drift-prone hand copy, and a Python emitter for ``make generate`` is a follow-up outside
this task's ``sdks/python`` seam — the concrete shapes are documented per method instead.
"""

from __future__ import annotations

from typing import Any

from ._common import enc, list_path, new_command_id, new_idempotency_key


class Responses:
    """The ``/v1/responses`` resource: create, retrieve, cancel, list, and the resumable stream."""

    def __init__(self, client: Any) -> None:
        self._client = client

    def create(
        self,
        request: dict[str, Any],
        *,
        idempotency_key: str | None = None,
        timeout_ms: float | None = None,
        max_retries: int | None = None,
    ) -> Any:
        """POST a new response; returns the queued handle. A stable Idempotency-Key is minted once
        and reused across transport retries, so a retried create settles exactly one response."""
        return self._client.request(
            "POST",
            "/v1/responses",
            body=request,
            idempotency_key=idempotency_key or new_idempotency_key(),
            timeout_ms=timeout_ms,
            max_retries=max_retries,
        )

    def retrieve(self, response_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        """Read a stored response. A 404 raises ``NotFoundError``; a 410 (purged) raises ``GoneError``."""
        return self._client.request(
            "GET", f"/v1/responses/{enc(response_id)}", timeout_ms=timeout_ms, max_retries=max_retries
        )

    def list(self, params: dict[str, Any] | None = None, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        """A tenant-scoped ``Page`` of run history with the shared opaque cursor + basic filters."""
        return self._client.request(
            "GET", list_path("/v1/responses", params), timeout_ms=timeout_ms, max_retries=max_retries
        )

    def cancel(self, response_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        """Best-effort cancellation of an in-flight response (accepted as 202)."""
        return self._client.request(
            "POST", f"/v1/responses/{enc(response_id)}/cancel", timeout_ms=timeout_ms, max_retries=max_retries
        )

    def stream(self, request: dict[str, Any], *, last_event_id: str | None = None, idempotency_key: str | None = None) -> Any:
        """Create a response and return a resumable, typed event stream over its session. Returns
        synchronously (sync client) / builds the async iterator (async client); the create fires
        lazily on first consumption. ``.final_response()`` resolves the canonical terminal Response."""

        def start() -> Any:
            return self.create(request, idempotency_key=idempotency_key)

        return self._client._new_response_stream(start, last_event_id=last_event_id)


class SessionCommands:
    """The durable command surface of a session (§9.2, §22.4): steer + interrupt."""

    def __init__(self, client: Any) -> None:
        self._client = client

    def steer(self, session_id: str, message: str, *, command_id: str | None = None, timeout_ms: float | None = None) -> Any:
        """Deliver a message at the next safe boundary WITHOUT interrupting the current step."""
        return self._send(session_id, "steer", message, command_id, timeout_ms)

    def interrupt(self, session_id: str, message: str, *, command_id: str | None = None, timeout_ms: float | None = None) -> Any:
        """Deliver a message that preempts the current step. Same durable, idempotent acceptance."""
        return self._send(session_id, "interrupt", message, command_id, timeout_ms)

    def _send(self, session_id: str, delivery: str, message: str, command_id: str | None, timeout_ms: float | None) -> Any:
        body = {
            "command_id": command_id or new_command_id(),
            "kind": "send_message",
            "delivery": delivery,
            "message": message,
        }
        # command_id carries idempotency server-side, so a network re-send settles exactly one command.
        return self._client.request(
            "POST", f"/v1/sessions/{enc(session_id)}/commands", body=body, idempotent=True, timeout_ms=timeout_ms
        )


class Sessions:
    """The ``/v1/sessions`` resource: create, retrieve, list, and the durable command surface."""

    def __init__(self, client: Any) -> None:
        self._client = client
        self.commands = SessionCommands(client)

    def create(self, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("POST", "/v1/sessions", timeout_ms=timeout_ms, max_retries=max_retries)

    def retrieve(self, session_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request(
            "GET", f"/v1/sessions/{enc(session_id)}", timeout_ms=timeout_ms, max_retries=max_retries
        )

    def list(self, params: dict[str, Any] | None = None, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request(
            "GET", list_path("/v1/sessions", params), timeout_ms=timeout_ms, max_retries=max_retries
        )


class Agents:
    """The ``/v1/agents`` read + publish resource."""

    def __init__(self, client: Any) -> None:
        self._client = client

    def list(self, params: dict[str, Any] | None = None, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", list_path("/v1/agents", params), timeout_ms=timeout_ms, max_retries=max_retries)

    def retrieve(self, agent_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"/v1/agents/{enc(agent_id)}", timeout_ms=timeout_ms, max_retries=max_retries)

    def list_revisions(self, agent_id: str, params: dict[str, Any] | None = None, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request(
            "GET", list_path(f"/v1/agents/{enc(agent_id)}/revisions", params), timeout_ms=timeout_ms, max_retries=max_retries
        )

    def publish_revision(self, agent_id: str, revision_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request(
            "POST", f"/v1/agents/{enc(agent_id)}/revisions/{enc(revision_id)}/publish", timeout_ms=timeout_ms, max_retries=max_retries
        )


class Artifacts:
    """The artifact retrieval resource (§22.6): metadata, an authenticated byte download, and a
    run-scoped list. A wrong-tenant or unknown id is an indistinguishable 404."""

    def __init__(self, client: Any) -> None:
        self._client = client

    def retrieve(self, artifact_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"/v1/artifacts/{enc(artifact_id)}", timeout_ms=timeout_ms, max_retries=max_retries)

    def list_for_response(self, response_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request(
            "GET", f"/v1/responses/{enc(response_id)}/artifacts", timeout_ms=timeout_ms, max_retries=max_retries
        )

    def download(self, artifact_id: str, *, timeout_ms: float | None = None) -> Any:
        """Open the authenticated byte stream for an artifact. Returns the raw, status-checked httpx
        streaming response (sync) / an awaitable of it (async): use ``.read()``/``.iter_bytes()`` (or
        the ``a``-prefixed async forms) and read the RFC 9530 ``Content-Digest`` header for integrity.
        ponytail: the raw response is the whole surface — a pre-signed URL + expiry is E13-H, and the
        convenience ``bytes()`` wrapper is not worth a sync/async fork the caller can do in one line."""
        return self._client.open_download(f"/v1/artifacts/{enc(artifact_id)}/content", timeout_ms=timeout_ms)


class _ListGet:
    """A read/LIST pair over the shared opaque cursor — the shape ``repository-bindings`` / ``tools`` /
    ``mcp-connections`` / ``triggers`` all take. ``_base`` is the collection path."""

    _base = ""

    def __init__(self, client: Any) -> None:
        self._client = client

    def list(self, params: dict[str, Any] | None = None, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", list_path(self._base, params), timeout_ms=timeout_ms, max_retries=max_retries)

    def retrieve(self, item_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"{self._base}/{enc(item_id)}", timeout_ms=timeout_ms, max_retries=max_retries)


class RepositoryBindings(_ListGet):
    _base = "/v1/repository-bindings"


class MCPConnections(_ListGet):
    _base = "/v1/mcp-connections"


class Triggers(_ListGet):
    _base = "/v1/triggers"


class Tools(_ListGet):
    """The extensibility tool lineages + the named tool-sets (§20.2, §28.2-28.4)."""

    _base = "/v1/tools"

    def list_sets(self, params: dict[str, Any] | None = None, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", list_path("/v1/tool-sets", params), timeout_ms=timeout_ms, max_retries=max_retries)


class SecretRefs:
    """The restart-less secret-ref write-path (§39.x, SEC-002/MCI-002). The VALUE is write-only —
    reads return metadata only. Requires a key with the ``provision`` capability."""

    def __init__(self, client: Any) -> None:
        self._client = client

    def create(self, name: str, value: str, *, timeout_ms: float | None = None) -> Any:
        return self._client.request("POST", "/v1/secret-refs", body={"name": name, "value": value}, timeout_ms=timeout_ms)

    def list(self, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", "/v1/secret-refs", timeout_ms=timeout_ms, max_retries=max_retries)

    def retrieve(self, name: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"/v1/secret-refs/{enc(name)}", timeout_ms=timeout_ms, max_retries=max_retries)

    def rotate(self, name: str, value: str, *, timeout_ms: float | None = None) -> Any:
        return self._client.request("POST", f"/v1/secret-refs/{enc(name)}/rotate", body={"value": value}, timeout_ms=timeout_ms)


class ModelRoutes:
    """The model-routing admin surface (E13 T8 write + E16 T1 read-back, MCI-006). List methods return
    the admin ``ListView`` envelope; a connection projection carries the secret REF name only, never a
    value. Requires a key with the ``provision`` capability."""

    def __init__(self, client: Any) -> None:
        self._client = client

    def create_connection(self, provider: str, secret_ref: str, *, timeout_ms: float | None = None) -> Any:
        return self._client.request(
            "POST", "/v1/model-connections", body={"provider": provider, "secret_ref": secret_ref}, timeout_ms=timeout_ms
        )

    def create_route(self, name: str, *, timeout_ms: float | None = None) -> Any:
        return self._client.request("POST", "/v1/model-routes", body={"name": name}, timeout_ms=timeout_ms)

    def create_revision(self, route_id: str, model: str, connection_id: str, *, timeout_ms: float | None = None) -> Any:
        return self._client.request(
            "POST", f"/v1/model-routes/{enc(route_id)}/revisions", body={"model": model, "connection_id": connection_id}, timeout_ms=timeout_ms
        )

    def publish_revision(self, route_id: str, revision_id: str, *, timeout_ms: float | None = None) -> Any:
        return self._client.request(
            "POST", f"/v1/model-routes/{enc(route_id)}/revisions/{enc(revision_id)}/publish", timeout_ms=timeout_ms
        )

    # --- E16 T1 read-back (the E13 T10 write-only gap) ---------------------------------------------

    def list_connections(self, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", "/v1/model-connections", timeout_ms=timeout_ms, max_retries=max_retries)

    def get_connection(self, connection_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"/v1/model-connections/{enc(connection_id)}", timeout_ms=timeout_ms, max_retries=max_retries)

    def list_routes(self, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", "/v1/model-routes", timeout_ms=timeout_ms, max_retries=max_retries)

    def get_route(self, route_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"/v1/model-routes/{enc(route_id)}", timeout_ms=timeout_ms, max_retries=max_retries)

    def list_revisions(self, route_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"/v1/model-routes/{enc(route_id)}/revisions", timeout_ms=timeout_ms, max_retries=max_retries)

    def get_revision(self, route_id: str, revision_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request(
            "GET", f"/v1/model-routes/{enc(route_id)}/revisions/{enc(revision_id)}", timeout_ms=timeout_ms, max_retries=max_retries
        )


class Organizations:
    """Administers tenants (§39.2). Creation is the one cross-tenant op — it provisions a SECOND tenant
    with no restart and discloses that tenant's admin key plaintext ONCE. Requires ``provision``."""

    def __init__(self, client: Any) -> None:
        self._client = client

    def create(self, display_name: str, *, timeout_ms: float | None = None) -> Any:
        return self._client.request("POST", "/v1/organizations", body={"display_name": display_name}, timeout_ms=timeout_ms)

    def list(self, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", "/v1/organizations", timeout_ms=timeout_ms, max_retries=max_retries)

    def retrieve(self, organization_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"/v1/organizations/{enc(organization_id)}", timeout_ms=timeout_ms, max_retries=max_retries)


class Projects:
    """Administers projects within the caller's organization, including the §14 config_policy write-path."""

    def __init__(self, client: Any) -> None:
        self._client = client

    def create(self, display_name: str, *, timeout_ms: float | None = None) -> Any:
        return self._client.request("POST", "/v1/projects", body={"display_name": display_name}, timeout_ms=timeout_ms)

    def list(self, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", "/v1/projects", timeout_ms=timeout_ms, max_retries=max_retries)

    def retrieve(self, project_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"/v1/projects/{enc(project_id)}", timeout_ms=timeout_ms, max_retries=max_retries)

    def update_policy(self, project_id: str, config_policy: Any, *, timeout_ms: float | None = None) -> Any:
        return self._client.request("PATCH", f"/v1/projects/{enc(project_id)}", body={"config_policy": config_policy}, timeout_ms=timeout_ms)


class ApiKeys:
    """Mints and revokes project-scoped keys. A key's plaintext is disclosed only on creation."""

    def __init__(self, client: Any) -> None:
        self._client = client

    def create(self, project_id: str, *, scopes: list[str] | None = None, expires_at: str | None = None, timeout_ms: float | None = None) -> Any:
        body: dict[str, Any] = {"project_id": project_id}
        if scopes is not None:
            body["scopes"] = scopes
        if expires_at is not None:
            body["expires_at"] = expires_at
        return self._client.request("POST", "/v1/api-keys", body=body, timeout_ms=timeout_ms)

    def list(self, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", "/v1/api-keys", timeout_ms=timeout_ms, max_retries=max_retries)

    def retrieve(self, key_id: str, *, timeout_ms: float | None = None, max_retries: int | None = None) -> Any:
        return self._client.request("GET", f"/v1/api-keys/{enc(key_id)}", timeout_ms=timeout_ms, max_retries=max_retries)

    def revoke(self, key_id: str, *, timeout_ms: float | None = None) -> Any:
        # revoke is naturally idempotent (revoked_at is monotonic).
        return self._client.request("POST", f"/v1/api-keys/{enc(key_id)}/revoke", idempotent=True, timeout_ms=timeout_ms)
