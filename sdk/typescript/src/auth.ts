// Capability-token auth for the mindd SDK.
//
// Every RPC requires a capability token carried in the gRPC metadata key
// "x-mindd-capability" with value "Bearer <token>". This interceptor
// attaches it to every outgoing call (unary and streaming alike).
import type { Interceptor } from "@connectrpc/connect";

/** gRPC metadata key the server reads the capability token from. */
export const CAPABILITY_HEADER = "x-mindd-capability";

/**
 * Build an interceptor that stamps `x-mindd-capability: Bearer <token>`
 * onto every request. Throws if the token is empty.
 */
export function capabilityInterceptor(token: string): Interceptor {
  if (!token) {
    throw new Error("capability token must not be empty");
  }
  const value = `Bearer ${token}`;
  return (next) => async (req) => {
    req.header.set(CAPABILITY_HEADER, value);
    return next(req);
  };
}
