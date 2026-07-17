import type { Problem } from "./generated/types.ts";

// ProblemCode is the stable RFC 9457 code set (problem.json known_codes). It stays OPEN
// (API-009): a code from a newer server is preserved as a plain string rather than
// rejected, while the literals keep editor completion for the known catalogue.
export type ProblemCode =
  | "invalid_request"
  | "invalid_state"
  | "unsupported_content"
  | "missing_idempotency_key"
  | "authentication_required"
  | "invalid_token"
  | "expired_token"
  | "permission_denied"
  | "capability_denied"
  | "policy_denied"
  | "region_denied"
  | "not_found"
  | "revision_conflict"
  | "idempotency_mismatch"
  | "idempotency_in_progress"
  | "active_run_conflict"
  | "lease_conflict"
  | "gone"
  | "idempotency_result_expired"
  | "retention_expired"
  | "precondition_failed"
  | "payload_too_large"
  | "context_too_large"
  | "schema_validation_failed"
  | "unsupported_model_capability"
  | "rate_limited"
  | "quota_exceeded"
  | "concurrency_exceeded"
  | "internal_error"
  | "provider_error"
  | "tool_transport_error"
  | "runner_error"
  | "capacity_unavailable"
  | "dependency_unavailable"
  | "maintenance"
  | "operation_timed_out"
  // The (string & {}) member keeps the literals in completion while still accepting any
  // string, so an unknown code from a newer server is preserved rather than rejected.
  | (string & {});

// PalaiError is the base for everything the SDK throws, so a caller can catch the whole
// surface with one `instanceof`.
export class PalaiError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = new.target.name;
  }
}

// PalaiConnectionError is a transport failure before any HTTP status was seen — a dropped
// socket, DNS failure, or an aborted per-attempt timeout. It is always retryable.
export class PalaiConnectionError extends PalaiError {
  readonly retryable = true;
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
  }
}

// PalaiAPIError is a typed RFC 9457 problem response. It carries the parsed Problem, the
// HTTP status, the stable code, the correlation request id, and whether the class of
// failure is retryable (§23.7).
export class PalaiAPIError extends PalaiError {
  readonly status: number;
  readonly code: ProblemCode;
  readonly problem: Problem;
  readonly requestId: string | undefined;
  readonly retryable: boolean;

  constructor(status: number, problem: Problem, requestId?: string) {
    super(problem.detail || problem.title || problem.code || `HTTP ${status}`);
    this.status = status;
    this.problem = problem;
    this.code = problem.code;
    this.requestId = problem.request_id || requestId;
    // The server's explicit retryable wins; otherwise fall back to the status class.
    this.retryable = problem.retryable ?? isRetryableStatus(status);
  }
}

// Family subclasses give ergonomic `instanceof` discrimination for the common HTTP
// classes without a class per stable code; the exact code is always on `.code`.
export class InvalidRequestError extends PalaiAPIError {}
export class AuthenticationError extends PalaiAPIError {}
export class PermissionDeniedError extends PalaiAPIError {}
export class NotFoundError extends PalaiAPIError {}
export class ConflictError extends PalaiAPIError {}
export class GoneError extends PalaiAPIError {}
export class RateLimitError extends PalaiAPIError {}
export class InternalServerError extends PalaiAPIError {}

// isRetryableStatus is the SDK's default retry predicate (§23.7): a request timeout, a
// rate limit, or any server-side 5xx may be retried; everything else is terminal.
export function isRetryableStatus(status: number): boolean {
  return status === 408 || status === 429 || status >= 500;
}

// apiErrorClass picks the family subclass for an HTTP status so callers can branch on the
// error type. An unmapped status falls back to the base PalaiAPIError.
function apiErrorClass(status: number): typeof PalaiAPIError {
  switch (status) {
    case 400:
    case 422:
      return InvalidRequestError;
    case 401:
      return AuthenticationError;
    case 403:
      return PermissionDeniedError;
    case 404:
      return NotFoundError;
    case 409:
      return ConflictError;
    case 410:
      return GoneError;
    case 429:
      return RateLimitError;
    default:
      return status >= 500 ? InternalServerError : PalaiAPIError;
  }
}

// errorForResponse builds the typed error for a non-2xx response. A well-formed
// application/problem+json body is parsed into a Problem; a body that is missing or not a
// problem document degrades to a synthesized Problem carrying the stable code the status
// implies, so a gateway's plain-text 502 still throws a typed, retryable error.
export function errorForResponse(status: number, bodyText: string, requestId?: string): PalaiAPIError {
  const problem = parseProblem(bodyText) ?? syntheticProblem(status, requestId);
  const Cls = apiErrorClass(status);
  return new Cls(status, problem, requestId);
}

function parseProblem(bodyText: string): Problem | null {
  if (!bodyText) {
    return null;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(bodyText);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) {
    return null;
  }
  const candidate = parsed as Partial<Problem>;
  if (typeof candidate.code !== "string" || typeof candidate.status !== "number") {
    return null;
  }
  return parsed as Problem;
}

function syntheticProblem(status: number, requestId?: string): Problem {
  const code = statusCode(status);
  return {
    type: `https://docs.palai.dev/problems/${code}`,
    title: code,
    status,
    code,
    request_id: requestId ?? "",
  };
}

function statusCode(status: number): ProblemCode {
  switch (status) {
    case 401:
      return "authentication_required";
    case 403:
      return "permission_denied";
    case 404:
      return "not_found";
    case 409:
      return "active_run_conflict";
    case 410:
      return "gone";
    case 429:
      return "rate_limited";
    case 503:
      return "capacity_unavailable";
    case 504:
      return "operation_timed_out";
    default:
      return status >= 500 ? "internal_error" : "invalid_request";
  }
}
