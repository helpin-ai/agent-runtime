from __future__ import annotations

from typing import Any, Dict, List, Optional

from pydantic import BaseModel, Field


class TargetDisplay(BaseModel):
    title: Optional[str] = None
    url: Optional[str] = None


class TargetRef(BaseModel):
    type: str
    id: str
    display: Optional[TargetDisplay] = None
    metadata: Dict[str, Any] = Field(default_factory=dict)


class SkillRef(BaseModel):
    skill_id: Optional[str] = None
    key: Optional[str] = None
    version: Optional[str] = None
    version_key: Optional[str] = None
    config: Dict[str, Any] = Field(default_factory=dict)


class Agent(BaseModel):
    id: Optional[str] = None
    app_id: str
    name: str
    runtime_kind: str = "native_sdk"
    provider: Optional[str] = None
    model: Optional[str] = None
    system_prompt: Optional[str] = None
    skills: List[SkillRef] = Field(default_factory=list)
    allowed_tools: List[str] = Field(default_factory=list)
    allowed_targets: List[str] = Field(default_factory=list)
    approval_mode: str = "never"
    default_invocation_mode: str = "autonomous"
    execution_config: Dict[str, Any] = Field(default_factory=dict)


class RunInput(BaseModel):
    instructions: Optional[str] = None
    allowed_tools: List[str] = Field(default_factory=list)
    trigger: Dict[str, Any] = Field(default_factory=dict)
    metadata: Dict[str, Any] = Field(default_factory=dict)
    context_summary: Optional[str] = None


class WorkspaceLease(BaseModel):
    id: str
    provider: Optional[str] = None
    root_path: str
    cleanup_policy: Optional[str] = None
    metadata: Dict[str, Any] = Field(default_factory=dict)


class AgentRun(BaseModel):
    id: str
    app_id: str
    agent_id: str
    target: TargetRef
    runtime_kind: str
    execution_mode: str
    invocation_mode: str
    external_actor_id: Optional[str] = None
    status: str
    pause_reason: str
    approval_state: str
    input: RunInput
    output_summary: Dict[str, Any] = Field(default_factory=dict)
    workspace_lease: Optional[WorkspaceLease] = None
    error_message: Optional[str] = None


class AgentRunMessage(BaseModel):
    id: str
    app_id: str
    run_id: str
    role: str
    content: str
    message_type: str
    sequence_no: int


class AgentRunArtifact(BaseModel):
    id: str
    app_id: str
    run_id: str
    artifact_type: str
    format: str
    storage_mode: str
    inline_content: Optional[str] = None
    metadata: Dict[str, Any] = Field(default_factory=dict)
    sequence_no: int


class AgentRunInteraction(BaseModel):
    id: str
    app_id: str
    run_id: str
    runtime_kind: str
    interaction_kind: str
    status: str
    title: Optional[str] = None
    summary: Optional[str] = None
    request_payload: Dict[str, Any] = Field(default_factory=dict)
    response_payload: Optional[Dict[str, Any]] = None


class ToolCall(BaseModel):
    id: str
    app_id: str
    run_id: str
    tool_name: str
    input: Dict[str, Any] = Field(default_factory=dict)
    output: Optional[Dict[str, Any]] = None
    error: Optional[str] = None
    mutating: bool = False
    approval_required: bool = False


class StartRunRequest(BaseModel):
    app_id: str
    agent_id: str
    target: TargetRef
    instructions: Optional[str] = None
    allowed_tools: List[str] = Field(default_factory=list)
    external_actor_id: Optional[str] = None
    mode: Optional[str] = None
    execution_mode: Optional[str] = None
    trigger: Dict[str, Any] = Field(default_factory=dict)
    metadata: Dict[str, Any] = Field(default_factory=dict)


class ResumeRunRequest(BaseModel):
    intent: str
    content: Optional[str] = None
    response_payload: Optional[Dict[str, Any]] = None
    external_actor_id: Optional[str] = None


class TargetContextRequest(BaseModel):
    app_id: str
    run_id: Optional[str] = None
    agent_id: Optional[str] = None
    target: TargetRef
    trigger: Dict[str, Any] = Field(default_factory=dict)
    metadata: Dict[str, Any] = Field(default_factory=dict)


class TargetContextResponse(BaseModel):
    target: TargetRef
    summary: Optional[str] = None
    data: Dict[str, Any] = Field(default_factory=dict)
    display: Optional[TargetDisplay] = None
