"""ProviderGateway: select, invoke, normalize, record, and emit degradation events."""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any, Protocol
from uuid import uuid4

from pydantic import ValidationError

from contracts.events import CapabilityDegraded, ProviderCallRecorded
from contracts.provider import ProviderCapability, ProviderError, ProviderResult

from .capabilities import ProviderRequest
from .cost import estimate_cost
from .registry import ProviderRegistration, ProviderRegistry


@dataclass(frozen=True, slots=True)
class ProviderCallRecord:
    call_id: str
    provider_id: str
    capability: ProviderCapability
    model: str
    case_id: str | None
    job_id: str | None
    latency_ms: int
    usage_json: dict[str, Any]
    cost_estimate: float | None
    status: str


class ProviderCallRecorder(Protocol):
    def record_provider_call(self, record: ProviderCallRecord) -> None:
        """Persist one provider_calls row."""


@dataclass(frozen=True, slots=True)
class ProviderGatewayResult:
    result: ProviderResult
    events: tuple[dict[str, Any], ...] = field(default_factory=tuple)


class ProviderGateway:
    def __init__(
        self,
        *,
        registry: ProviderRegistry,
        recorder: ProviderCallRecorder | None = None,
    ) -> None:
        self._registry = registry
        self._recorder = recorder

    async def call(
        self,
        request: ProviderRequest,
        *,
        provider_id: str | None = None,
        require_raw_transcript: bool = False,
    ) -> ProviderGatewayResult:
        first = self._registry.find(
            request.capability,
            provider_id=provider_id,
            supports_raw_transcript=True if require_raw_transcript else None,
        )
        events: list[dict[str, Any]] = []
        attempts = (first, *self._registry.fallback_chain(first.descriptor))
        last_result: ProviderResult | None = None
        for index, registration in enumerate(attempts):
            result = await self._invoke_one(registration, request)
            last_result = result
            events.append(_provider_call_event(result, request))
            if result.error is None:
                return ProviderGatewayResult(result=result, events=tuple(events))
            fallback = (
                attempts[index + 1].descriptor.provider_id if index + 1 < len(attempts) else None
            )
            if fallback is not None:
                events.append(_capability_degraded_event(result, request, fallback=fallback))
        if last_result is None:
            raise RuntimeError("provider gateway attempted no providers")
        return ProviderGatewayResult(result=last_result, events=tuple(events))

    async def _invoke_one(
        self,
        registration: ProviderRegistration,
        request: ProviderRequest,
    ) -> ProviderResult:
        started = time.monotonic()
        request_id = request.request_id or f"provider_req_{uuid4().hex}"
        try:
            raw = await registration.adapter.invoke(
                request.model_copy(update={"request_id": request_id})
            )
            latency_ms = _elapsed_ms(started)
            result = _normalize_result(
                raw,
                registration=registration,
                request=request,
                request_id=request_id,
                latency_ms=latency_ms,
            )
        except Exception as exc:
            latency_ms = _elapsed_ms(started)
            result = ProviderResult(
                provider_id=registration.descriptor.provider_id,
                capability=request.capability,
                request_id=request_id,
                model=request.model or registration.descriptor.provider_id,
                latency_ms=latency_ms,
                error=ProviderError(
                    error_code="provider_exception",
                    message=str(exc),
                    retryable=True,
                    details={"exception_type": type(exc).__name__},
                ),
            )
        self._record(result, request)
        return result

    def _record(self, result: ProviderResult, request: ProviderRequest) -> None:
        if self._recorder is None:
            return
        self._recorder.record_provider_call(
            ProviderCallRecord(
                call_id=result.request_id,
                provider_id=result.provider_id,
                capability=result.capability,
                model=result.model,
                case_id=request.case_id,
                job_id=request.job_id,
                latency_ms=result.latency_ms,
                usage_json=result.usage,
                cost_estimate=estimate_cost(result.usage),
                status="failed" if result.error is not None else "succeeded",
            )
        )


def _normalize_result(
    raw: ProviderResult | dict[str, Any],
    *,
    registration: ProviderRegistration,
    request: ProviderRequest,
    request_id: str,
    latency_ms: int,
) -> ProviderResult:
    data = raw.model_dump(mode="python") if isinstance(raw, ProviderResult) else dict(raw)
    data.setdefault("provider_id", registration.descriptor.provider_id)
    data.setdefault("capability", request.capability)
    data.setdefault("request_id", request_id)
    data.setdefault("model", request.model or registration.descriptor.provider_id)
    data.setdefault("latency_ms", latency_ms)
    data.setdefault("usage", {})
    data.setdefault("normalized_output", {})
    data.setdefault("warnings", [])
    data.setdefault("raw_ref", None)
    data.setdefault("error", None)
    try:
        return ProviderResult.model_validate(data)
    except ValidationError as exc:
        return ProviderResult(
            provider_id=registration.descriptor.provider_id,
            capability=request.capability,
            request_id=request_id,
            model=request.model or registration.descriptor.provider_id,
            latency_ms=latency_ms,
            error=ProviderError(
                error_code="provider_result_schema_error",
                message="provider result failed ProviderResult validation",
                retryable=False,
                details={"validation_errors": exc.errors(include_url=False)},
            ),
        )


def _provider_call_event(result: ProviderResult, request: ProviderRequest) -> dict[str, Any]:
    return ProviderCallRecorded(
        provider_call_id=result.request_id,
        project_id=None,
        case_id=request.case_id,
        payload={
            "provider_id": result.provider_id,
            "capability": result.capability,
            "model": result.model,
            "job_id": request.job_id,
            "status": "failed" if result.error is not None else "succeeded",
        },
    ).model_dump(mode="json")


def _capability_degraded_event(
    result: ProviderResult,
    request: ProviderRequest,
    *,
    fallback: str,
) -> dict[str, Any]:
    reason = "provider failed"
    if result.error is not None:
        reason = f"{result.error.error_code}: {result.error.message}"
    return CapabilityDegraded(
        degradation_id=f"degraded_{result.request_id}",
        case_id=request.case_id,
        capability=result.capability,
        provider_id=result.provider_id,
        reason=reason,
        fallback=fallback,
        payload={"job_id": request.job_id},
    ).model_dump(mode="json")


def _elapsed_ms(started: float) -> int:
    return max(0, int((time.monotonic() - started) * 1000))
