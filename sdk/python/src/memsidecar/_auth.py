"""gRPC client interceptor that injects the capability token on every call."""
from __future__ import annotations

from typing import Any, Callable, Iterator

import grpc


CAPABILITY_HEADER = "x-memsidecar-capability"


def _attach_capability(
    client_call_details: grpc.ClientCallDetails,
    token: str,
) -> grpc.ClientCallDetails:
    metadata = list(client_call_details.metadata or [])
    metadata.append((CAPABILITY_HEADER, f"Bearer {token}"))
    return _ClientCallDetails(
        method=client_call_details.method,
        timeout=client_call_details.timeout,
        metadata=metadata,
        credentials=client_call_details.credentials,
        wait_for_ready=client_call_details.wait_for_ready,
        compression=client_call_details.compression,
    )


class _ClientCallDetails(grpc.ClientCallDetails):
    """Mutable copy of ClientCallDetails. The protocol type doesn't expose a
    replace() so we build a tiny named-tuple-like substitute."""

    __slots__ = (
        "method",
        "timeout",
        "metadata",
        "credentials",
        "wait_for_ready",
        "compression",
    )

    def __init__(self, method, timeout, metadata, credentials, wait_for_ready, compression):
        self.method = method
        self.timeout = timeout
        self.metadata = metadata
        self.credentials = credentials
        self.wait_for_ready = wait_for_ready
        self.compression = compression


class CapabilityInterceptor(
    grpc.UnaryUnaryClientInterceptor,
    grpc.UnaryStreamClientInterceptor,
    grpc.StreamUnaryClientInterceptor,
    grpc.StreamStreamClientInterceptor,
):
    """Adds `x-memsidecar-capability: Bearer <token>` to every outgoing call."""

    def __init__(self, token: str):
        if not token:
            raise ValueError("capability token must not be empty")
        self._token = token

    def _intercept(
        self,
        continuation: Callable[..., Any],
        client_call_details: grpc.ClientCallDetails,
        request_or_iterator: Any,
    ) -> Any:
        return continuation(_attach_capability(client_call_details, self._token), request_or_iterator)

    def intercept_unary_unary(self, continuation, client_call_details, request):
        return self._intercept(continuation, client_call_details, request)

    def intercept_unary_stream(self, continuation, client_call_details, request) -> Iterator[Any]:
        return self._intercept(continuation, client_call_details, request)

    def intercept_stream_unary(self, continuation, client_call_details, request_iterator):
        return self._intercept(continuation, client_call_details, request_iterator)

    def intercept_stream_stream(self, continuation, client_call_details, request_iterator) -> Iterator[Any]:
        return self._intercept(continuation, client_call_details, request_iterator)
