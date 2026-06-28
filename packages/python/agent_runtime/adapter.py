from __future__ import annotations

from typing import Awaitable, Callable, List, Optional, Union

from .models import (
    CleanupWorkspaceRequest,
    CommandExecutionRequest,
    CommandExecutionResponse,
    FinalizeWorkspaceRequest,
    FinalizeWorkspaceResult,
    PrepareWorkspaceRequest,
    RepositoryWorkspaceSpec,
    SkillLookupRequest,
    TargetContextRequest,
    TargetContextResponse,
    ToolCallRequest,
    ToolDefinition,
    ToolResult,
    WorkspaceLease,
    WorkspaceSkill,
)

TargetContextHandler = Callable[
    [TargetContextRequest],
    Union[TargetContextResponse, Awaitable[TargetContextResponse]],
]
MCPListToolsHandler = Callable[
    [],
    Union[List[ToolDefinition], Awaitable[List[ToolDefinition]]],
]
MCPCallToolHandler = Callable[
    [ToolCallRequest],
    Union[ToolResult, Awaitable[ToolResult]],
]
CommandExecutionHandler = Callable[
    [CommandExecutionRequest],
    Union[CommandExecutionResponse, Awaitable[CommandExecutionResponse]],
]
PrepareWorkspaceHandler = Callable[
    [PrepareWorkspaceRequest],
    Union[WorkspaceLease, Awaitable[WorkspaceLease]],
]
FinalizeWorkspaceHandler = Callable[
    [FinalizeWorkspaceRequest],
    Union[FinalizeWorkspaceResult, Awaitable[FinalizeWorkspaceResult]],
]
CleanupWorkspaceHandler = Callable[
    [CleanupWorkspaceRequest],
    Union[None, Awaitable[None]],
]
RepositorySpecHandler = Callable[
    [PrepareWorkspaceRequest],
    Union[RepositoryWorkspaceSpec, Awaitable[RepositoryWorkspaceSpec]],
]
SkillLookupHandler = Callable[
    [SkillLookupRequest],
    Union[Optional[WorkspaceSkill], Awaitable[Optional[WorkspaceSkill]]],
]
SkillPackageObjectHandler = Callable[
    [str],
    Union[bytes, Awaitable[bytes]],
]


def verify_bearer_token(authorization: Optional[str], expected_token: Optional[str]) -> None:
    if not expected_token:
        return
    value = (authorization or "").strip()
    if value.lower().startswith("bearer "):
        value = value[7:].strip()
    if value != expected_token:
        raise PermissionError("unauthorized")


def create_fastapi_target_context_router(handler: TargetContextHandler, token: Optional[str] = None):
    try:
        from fastapi import APIRouter, Header, HTTPException
    except ImportError as exc:
        raise RuntimeError("Install agent-runtime[fastapi] to use FastAPI adapter helpers") from exc

    router = APIRouter()

    @router.post("/agent-runtime/target-context", response_model=TargetContextResponse)
    async def target_context(
        request: TargetContextRequest,
        authorization: Optional[str] = Header(default=None),
    ):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        result = handler(request)
        return await _resolve(result)

    return router


def create_fastapi_mcp_provider_router(
    list_tools: MCPListToolsHandler,
    call_tool: MCPCallToolHandler,
    token: Optional[str] = None,
):
    try:
        from fastapi import APIRouter, Header, HTTPException
    except ImportError as exc:
        raise RuntimeError("Install agent-runtime[fastapi] to use FastAPI adapter helpers") from exc

    router = APIRouter()

    @router.get("/tools")
    async def tools(authorization: Optional[str] = Header(default=None)):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        return {"tools": await _resolve(list_tools())}

    @router.post("/call", response_model=ToolResult)
    async def call(request: ToolCallRequest, authorization: Optional[str] = Header(default=None)):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        return await _resolve(call_tool(request))

    return router


def create_fastapi_command_executor_router(handler: CommandExecutionHandler, token: Optional[str] = None):
    try:
        from fastapi import APIRouter, Header, HTTPException
    except ImportError as exc:
        raise RuntimeError("Install agent-runtime[fastapi] to use FastAPI adapter helpers") from exc

    router = APIRouter()

    @router.post("/execute", response_model=CommandExecutionResponse)
    async def execute(request: CommandExecutionRequest, authorization: Optional[str] = Header(default=None)):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        return await _resolve(handler(request))

    return router


def create_fastapi_workspace_provider_router(
    prepare: PrepareWorkspaceHandler,
    finalize: FinalizeWorkspaceHandler,
    cleanup: CleanupWorkspaceHandler,
    repository_spec: Optional[RepositorySpecHandler] = None,
    token: Optional[str] = None,
):
    try:
        from fastapi import APIRouter, Header, HTTPException
    except ImportError as exc:
        raise RuntimeError("Install agent-runtime[fastapi] to use FastAPI adapter helpers") from exc

    router = APIRouter()

    @router.post("/prepare", response_model=WorkspaceLease)
    async def prepare_workspace(request: PrepareWorkspaceRequest, authorization: Optional[str] = Header(default=None)):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        return await _resolve(prepare(request))

    @router.post("/finalize", response_model=FinalizeWorkspaceResult)
    async def finalize_workspace(request: FinalizeWorkspaceRequest, authorization: Optional[str] = Header(default=None)):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        return await _resolve(finalize(request))

    @router.post("/cleanup")
    async def cleanup_workspace(request: CleanupWorkspaceRequest, authorization: Optional[str] = Header(default=None)):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        await _resolve(cleanup(request))
        return {}

    if repository_spec is not None:
        @router.post("/repository-spec", response_model=RepositoryWorkspaceSpec)
        async def resolve_repository_spec(request: PrepareWorkspaceRequest, authorization: Optional[str] = Header(default=None)):
            try:
                verify_bearer_token(authorization, token)
            except PermissionError:
                raise HTTPException(status_code=401, detail="unauthorized")
            return await _resolve(repository_spec(request))

    return router


def create_fastapi_workspace_skill_lookup_router(
    get_by_id: SkillLookupHandler,
    get_active_by_key: SkillLookupHandler,
    token: Optional[str] = None,
):
    try:
        from fastapi import APIRouter, Header, HTTPException, Response
    except ImportError as exc:
        raise RuntimeError("Install agent-runtime[fastapi] to use FastAPI adapter helpers") from exc

    router = APIRouter()

    @router.post("/by-id", response_model=WorkspaceSkill)
    async def by_id(request: SkillLookupRequest, authorization: Optional[str] = Header(default=None)):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        skill = await _resolve(get_by_id(request))
        if skill is None:
            return Response(status_code=204)
        return skill

    @router.post("/active-by-key", response_model=WorkspaceSkill)
    async def active_by_key(request: SkillLookupRequest, authorization: Optional[str] = Header(default=None)):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        skill = await _resolve(get_active_by_key(request))
        if skill is None:
            return Response(status_code=204)
        return skill

    return router


def create_fastapi_skill_package_router(
    get_object: SkillPackageObjectHandler,
    token: Optional[str] = None,
):
    try:
        from fastapi import APIRouter, Header, HTTPException, Response
    except ImportError as exc:
        raise RuntimeError("Install agent-runtime[fastapi] to use FastAPI adapter helpers") from exc

    router = APIRouter()

    @router.get("/objects/{object_key:path}")
    async def object_by_key(
        object_key: str,
        authorization: Optional[str] = Header(default=None),
    ):
        try:
            verify_bearer_token(authorization, token)
        except PermissionError:
            raise HTTPException(status_code=401, detail="unauthorized")
        payload = await _resolve(get_object(object_key))
        if not payload:
            raise HTTPException(status_code=404, detail="skill package object not found")
        return Response(content=payload, media_type="application/zip")

    return router


async def _resolve(value):
    if hasattr(value, "__await__"):
        return await value
    return value
