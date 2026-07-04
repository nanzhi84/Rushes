"""ProviderDescriptor registry and fallback-chain discovery."""

from __future__ import annotations

from dataclasses import dataclass

from contracts.provider import ProviderCapability, ProviderDescriptor

from .capabilities import ProviderAdapter


@dataclass(frozen=True, slots=True)
class ProviderRegistration:
    descriptor: ProviderDescriptor
    adapter: ProviderAdapter


class ProviderRegistry:
    def __init__(self) -> None:
        self._providers: dict[str, ProviderRegistration] = {}

    def register(self, descriptor: ProviderDescriptor, adapter: ProviderAdapter) -> None:
        if descriptor.provider_id in self._providers:
            raise ValueError(f"provider already registered: {descriptor.provider_id}")
        self._providers[descriptor.provider_id] = ProviderRegistration(descriptor, adapter)

    def require(self, provider_id: str) -> ProviderRegistration:
        registration = self._providers.get(provider_id)
        if registration is None:
            raise KeyError(f"provider is not registered: {provider_id}")
        return registration

    def find(
        self,
        capability: ProviderCapability,
        *,
        provider_id: str | None = None,
        supports_raw_transcript: bool | None = None,
    ) -> ProviderRegistration:
        if provider_id is not None:
            registration = self.require(provider_id)
            if not _matches(registration.descriptor, capability, supports_raw_transcript):
                raise ValueError(f"provider {provider_id} does not support {capability}")
            return registration
        candidates = [
            registration
            for registration in self._providers.values()
            if _matches(registration.descriptor, capability, supports_raw_transcript)
        ]
        if not candidates:
            raise KeyError(f"no provider registered for capability: {capability}")
        return sorted(
            candidates,
            key=lambda registration: (
                registration.descriptor.priority,
                registration.descriptor.provider_id,
            ),
        )[0]

    def fallback_chain(self, descriptor: ProviderDescriptor) -> tuple[ProviderRegistration, ...]:
        chain: list[ProviderRegistration] = []
        seen = {descriptor.provider_id}
        frontier = list(descriptor.fallback_provider_ids)
        while frontier:
            provider_id = frontier.pop(0)
            if provider_id in seen:
                continue
            seen.add(provider_id)
            registration = self._providers.get(provider_id)
            if registration is None:
                continue
            chain.append(registration)
            frontier.extend(registration.descriptor.fallback_provider_ids)
        return tuple(chain)

    def list(self) -> tuple[ProviderDescriptor, ...]:
        return tuple(
            registration.descriptor
            for registration in sorted(
                self._providers.values(),
                key=lambda item: item.descriptor.provider_id,
            )
        )


def _matches(
    descriptor: ProviderDescriptor,
    capability: ProviderCapability,
    supports_raw_transcript: bool | None,
) -> bool:
    if capability not in descriptor.capabilities:
        return False
    if supports_raw_transcript is not None:
        return descriptor.supports_raw_transcript is supports_raw_transcript
    return True
