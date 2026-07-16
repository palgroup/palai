"""Code generated from the canonical CanonicalIdentifiers schema; DO NOT EDIT."""

from __future__ import annotations

import re
from typing import NewType

ArtifactId = NewType("ArtifactId", str)
AttemptId = NewType("AttemptId", str)
CommandId = NewType("CommandId", str)
EventId = NewType("EventId", str)
FrameId = NewType("FrameId", str)
MessageId = NewType("MessageId", str)
ModelRequestId = NewType("ModelRequestId", str)
OpaqueId = NewType("OpaqueId", str)
OrganizationId = NewType("OrganizationId", str)
ProjectId = NewType("ProjectId", str)
RequestId = NewType("RequestId", str)
ResponseId = NewType("ResponseId", str)
RunId = NewType("RunId", str)
SessionId = NewType("SessionId", str)
ToolCallId = NewType("ToolCallId", str)
WorkspaceId = NewType("WorkspaceId", str)

ARTIFACT_ID_PATTERN = re.compile(r"^art_[A-Za-z0-9_-]+$")
ATTEMPT_ID_PATTERN = re.compile(r"^att_[A-Za-z0-9_-]+$")
COMMAND_ID_PATTERN = re.compile(r"^cmd_[A-Za-z0-9_-]+$")
EVENT_ID_PATTERN = re.compile(r"^evt_[A-Za-z0-9_-]+$")
FRAME_ID_PATTERN = re.compile(r"^frm_[A-Za-z0-9_-]+$")
MESSAGE_ID_PATTERN = re.compile(r"^msg_[A-Za-z0-9_-]+$")
MODEL_REQUEST_ID_PATTERN = re.compile(r"^mreq_[A-Za-z0-9_-]+$")
OPAQUE_ID_PATTERN = re.compile(r"^[a-z][a-z0-9]{1,11}_[A-Za-z0-9_-]+$")
ORGANIZATION_ID_PATTERN = re.compile(r"^org_[A-Za-z0-9_-]+$")
PROJECT_ID_PATTERN = re.compile(r"^prj_[A-Za-z0-9_-]+$")
REQUEST_ID_PATTERN = re.compile(r"^req_[A-Za-z0-9_-]+$")
RESPONSE_ID_PATTERN = re.compile(r"^resp_[A-Za-z0-9_-]+$")
RUN_ID_PATTERN = re.compile(r"^run_[A-Za-z0-9_-]+$")
SESSION_ID_PATTERN = re.compile(r"^ses_[A-Za-z0-9_-]+$")
TOOL_CALL_ID_PATTERN = re.compile(r"^tcall_[A-Za-z0-9_-]+$")
WORKSPACE_ID_PATTERN = re.compile(r"^wksp_[A-Za-z0-9_-]+$")


def is_artifact_id(value: str) -> bool:
    return ARTIFACT_ID_PATTERN.match(value) is not None


def is_attempt_id(value: str) -> bool:
    return ATTEMPT_ID_PATTERN.match(value) is not None


def is_command_id(value: str) -> bool:
    return COMMAND_ID_PATTERN.match(value) is not None


def is_event_id(value: str) -> bool:
    return EVENT_ID_PATTERN.match(value) is not None


def is_frame_id(value: str) -> bool:
    return FRAME_ID_PATTERN.match(value) is not None


def is_message_id(value: str) -> bool:
    return MESSAGE_ID_PATTERN.match(value) is not None


def is_model_request_id(value: str) -> bool:
    return MODEL_REQUEST_ID_PATTERN.match(value) is not None


def is_opaque_id(value: str) -> bool:
    return OPAQUE_ID_PATTERN.match(value) is not None


def is_organization_id(value: str) -> bool:
    return ORGANIZATION_ID_PATTERN.match(value) is not None


def is_project_id(value: str) -> bool:
    return PROJECT_ID_PATTERN.match(value) is not None


def is_request_id(value: str) -> bool:
    return REQUEST_ID_PATTERN.match(value) is not None


def is_response_id(value: str) -> bool:
    return RESPONSE_ID_PATTERN.match(value) is not None


def is_run_id(value: str) -> bool:
    return RUN_ID_PATTERN.match(value) is not None


def is_session_id(value: str) -> bool:
    return SESSION_ID_PATTERN.match(value) is not None


def is_tool_call_id(value: str) -> bool:
    return TOOL_CALL_ID_PATTERN.match(value) is not None


def is_workspace_id(value: str) -> bool:
    return WORKSPACE_ID_PATTERN.match(value) is not None

