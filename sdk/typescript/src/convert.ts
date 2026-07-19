// Small conversions between JS-native types and the protobuf well-known types
// used across the block APIs.
import { create } from "@bufbuild/protobuf";
import type { Duration, Timestamp } from "@bufbuild/protobuf/wkt";
import { DurationSchema, timestampFromDate } from "@bufbuild/protobuf/wkt";

/** A `Date` becomes a `Timestamp`; `undefined` passes through. */
export function toTimestamp(d: Date | undefined): Timestamp | undefined {
  return d === undefined ? undefined : timestampFromDate(d);
}

/**
 * Seconds (fractional allowed) become a `Duration`; `undefined` passes through.
 * Sub-second precision is carried in nanos.
 */
export function toDuration(seconds: number | undefined): Duration | undefined {
  if (seconds === undefined) {
    return undefined;
  }
  const whole = Math.trunc(seconds);
  const nanos = Math.round((seconds - whole) * 1e9);
  return create(DurationSchema, { seconds: BigInt(whole), nanos });
}

/** Accept `number` or `bigint` for a uint64 field and normalize to `bigint`. */
export function toU64(v: number | bigint | undefined): bigint | undefined {
  return v === undefined ? undefined : BigInt(v);
}
