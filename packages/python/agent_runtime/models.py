from __future__ import annotations

from datetime import datetime
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
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None


class MCPProviderConfig(BaseModel):
    name: str
    transport: str = "http"
    url: Optional[str] = None
    token: Optional[str] = None
    command: Optional[str] = None
    args: List[str] = Field(default_factory=list)
    env: Dict[str, str] = Field(default_factory=dict)
    tool_prefix: Optional[str] = None
    allowed_tools: List[str] = Field(default_factory=list)


class CommandProviderConfig(BaseModel):
    transport: str = "http"
    base_url: str
    token: Optional[str] = None


class WorkspaceProviderConfig(BaseModel):
    transport: str = "http"
    base_url: str
    token: Optional[str] = None
    root_dir: Optional[str] = None


class SkillProviderConfig(BaseModel):
    transport: str = "http"
    base_url: str
    token: Optional[str] = None
    package_base_url: Optional[str] = None
    package_token: Optional[str] = None


class AppConfigApp(BaseModel):
    app_id: str
    context_endpoint: Optional[str] = None
    context_token: Optional[str] = None
    mcp_providers: List[MCPProviderConfig] = Field(default_factory=list)
    command_provider: Optional[CommandProviderConfig] = None
    workspace_provider: Optional[WorkspaceProviderConfig] = None
    skill_provider: Optional[SkillProviderConfig] = None


class AppConfig(BaseModel):
    apps: List[AppConfigApp] = Field(default_factory=list)


class RunInput(BaseModel):
    instructions: Optional[str] = None
    allowed_tools: List[str] = Field(default_factory=list)
    trigger: Dict[str, Any] = Field(default_factory=dict)
    metadata: Dict[str, Any] = Field(default_factory=dict)
    context_summary: Optional[str] = None
    turn_policy: Dict[str, Any] = Field(default_factory=dict)


class WorkspaceLease(BaseModel):
    id: str
    provider: Optional[str] = None
    root_path: str
    cleanup_policy: Optional[str] = None
    metadata: Dict[str, Any] = Field(default_factory=dict)


class RepositoryAuth(BaseModel):
    type: Optional[str] = None
    token: Optional[str] = None
    username: Optional[str] = None
    password: Optional[str] = None
    extra_header: Optional[str] = None
    env: Dict[str, str] = Field(default_factory=dict)


class GitIdentity(BaseModel):
    name: Optional[str] = None
    email: Optional[str] = None


class RepositoryWorkspaceSpec(BaseModel):
    provider: Optional[str] = None
    clone_url: str
    auth: Optional[RepositoryAuth] = None
    base_branch: Optional[str] = None
    work_branch: Optional[str] = None
    commit_identity: Optional[GitIdentity] = None
    finalize_policy: Optional[str] = None
    metadata: Dict[str, Any] = Field(default_factory=dict)


class PrepareWorkspaceRequest(BaseModel):
    app_id: str
    run_id: str
    agent_id: str
    runtime_kind: str
    target: TargetRef
    target_context: Optional["TargetContextResponse"] = None
    instructions: Optional[str] = None
    trigger: Dict[str, Any] = Field(default_factory=dict)
    metadata: Dict[str, Any] = Field(default_factory=dict)
    workspace_mode: str
    execution_config: Dict[str, Any] = Field(default_factory=dict)


class FinalizeWorkspaceRequest(BaseModel):
    app_id: str
    run_id: str
    agent_id: str
    runtime_kind: str
    target: TargetRef
    lease: WorkspaceLease
    repository: Optional[RepositoryWorkspaceSpec] = None
    outcome: str
    error_message: Optional[str] = None
    output_summary: Dict[str, Any] = Field(default_factory=dict)


class FinalizeWorkspaceResult(BaseModel):
    output_summary: Dict[str, Any] = Field(default_factory=dict)
    metadata: Dict[str, Any] = Field(default_factory=dict)


class CleanupWorkspaceRequest(BaseModel):
    app_id: str
    run_id: str
    agent_id: str
    runtime_kind: str
    target: TargetRef
    lease: WorkspaceLease
    repository: Optional[RepositoryWorkspaceSpec] = None
    reason: Optional[str] = None


class AgentRun(BaseModel):
    id: str
    app_id: str
    host_run_id: Optional[str] = None
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
    started_at: Optional[datetime] = None
    completed_at: Optional[datetime] = None
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None


class AgentRunMessage(BaseModel):
    id: str
    app_id: str
    run_id: str
    role: str
    content: str
    message_type: str
    content_blocks: Optional[Any] = None
    tool_invocations: Optional[Any] = None
    sequence_no: int
    created_at: Optional[datetime] = None


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
    created_at: Optional[datetime] = None


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
    resolved_by_external_id: Optional[str] = None
    resolved_at: Optional[datetime] = None
    created_at: Optional[datetime] = None
    updated_at: Optional[datetime] = None


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
    created_at: Optional[datetime] = None


class ToolDefinition(BaseModel):
    name: str
    description: str
    category: Optional[str] = None
    input_schema: Dict[str, Any] = Field(default_factory=dict)
    mutating: bool = False
    supported_target_types: List[str] = Field(default_factory=list)


class ToolContentItem(BaseModel):
    type: str
    text: Optional[str] = None


class ToolResult(BaseModel):
    content: List[ToolContentItem] = Field(default_factory=list)
    is_error: bool = False
    approval_required: bool = False
    interaction_id: Optional[str] = None


class CommandExecutionContext(BaseModel):
    app_id: str
    run_id: Optional[str] = None
    agent_id: Optional[str] = None
    external_actor_id: Optional[str] = None
    workspace_id: Optional[str] = None
    target_type: Optional[str] = None
    target_id: Optional[str] = None
    target: TargetRef
    run_input_metadata: Dict[str, Any] = Field(default_factory=dict)
    target_metadata: Dict[str, Any] = Field(default_factory=dict)
    workspace_metadata: Dict[str, Any] = Field(default_factory=dict)


class CommandExecutionRequest(BaseModel):
    meta: CommandExecutionContext
    command_name: str
    input: Dict[str, Any] = Field(default_factory=dict)


class ToolCallRequest(BaseModel):
    tool_name: str
    input: Dict[str, Any] = Field(default_factory=dict)
    meta: Optional[CommandExecutionContext] = None


class CommandExecutionResponse(BaseModel):
    output: Optional[Any] = None
    error: Optional[str] = None


class CodexAuthState(BaseModel):
    provider: Optional[str] = None
    auth_mode: Optional[str] = None
    state: str
    login_id: Optional[str] = None
    auth_url: Optional[str] = None
    verification_url: Optional[str] = None
    user_code: Optional[str] = None
    plan_type: Optional[str] = None
    error: Optional[str] = None
    updated_at: Optional[datetime] = None


class StartRunRequest(BaseModel):
    app_id: str
    host_run_id: Optional[str] = None
    agent_id: str
    target: TargetRef
    instructions: Optional[str] = None
    allowed_tools: List[str] = Field(default_factory=list)
    external_actor_id: Optional[str] = None
    mode: Optional[str] = None
    execution_mode: Optional[str] = None
    trigger: Dict[str, Any] = Field(default_factory=dict)
    metadata: Dict[str, Any] = Field(default_factory=dict)
    turn_policy: Dict[str, Any] = Field(default_factory=dict)


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


class InteractionTransport(BaseModel):
    type: Optional[str] = None
    tool_name: Optional[str] = None
    block_label: Optional[str] = None


class InteractionContract(BaseModel):
    kind: Optional[str] = None
    schema_: Optional[str] = Field(default=None, alias="schema")
    transports: Dict[str, InteractionTransport] = Field(default_factory=dict)


class SkillPolicy(BaseModel):
    allow_implicit_invocation: Optional[bool] = None
    completion_requires_interaction_kinds: List[str] = Field(default_factory=list)
    interaction_contracts: List[InteractionContract] = Field(default_factory=list)


class SkillInterface(BaseModel):
    display_name: Optional[str] = None
    short_description: Optional[str] = None
    icon_small: Optional[str] = None
    icon_large: Optional[str] = None
    brand_color: Optional[str] = None
    default_prompt: Optional[str] = None


class WorkspaceSkill(BaseModel):
    id: str
    key: str
    version_key: str
    title: str
    description: str
    source_kind: str
    instructions: str
    required_tools: List[str] = Field(default_factory=list)
    supported_runtimes: List[str] = Field(default_factory=list)
    policy: SkillPolicy = Field(default_factory=SkillPolicy)
    interface: SkillInterface = Field(default_factory=SkillInterface)
    package_object_key: Optional[str] = None
    package_file_name: Optional[str] = None
    package_checksum: Optional[str] = None
    package_size: Optional[int] = None
    is_archived: bool = False


class SkillLookupRequest(BaseModel):
    app_id: str
    agent_id: Optional[str] = None
    run_id: Optional[str] = None
    target: Optional[TargetRef] = None
    trigger: Dict[str, Any] = Field(default_factory=dict)
    metadata: Dict[str, Any] = Field(default_factory=dict)
    skill_id: Optional[str] = None
    key: Optional[str] = None
