from __future__ import annotations

from typing import Awaitable, Callable, Optional, Union

from .models import TargetContextRequest, TargetContextResponse

TargetContextHandler = Callable[
    [TargetContextRequest],
    Union[TargetContextResponse, Awaitable[TargetContextResponse]],
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
        if hasattr(result, "__await__"):
            result = await result
        return result

    return router
