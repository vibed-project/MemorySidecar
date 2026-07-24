// Package redis implements the kv.Driver contract on Redis or any
// wire-compatible server (Valkey, KeyDog, etc.) via the RESP protocol.
//
// Layout. Each record is a Redis hash at key
//
//	<prefix>h:<namespace>\x1e<key>
//
// with fields v (value bytes), ct (content_type), md (metadata JSON), ver
// (version), cr (created_at unix-ms) and ex (expires_at unix-ms, 0 = none). A
// per-namespace sorted set at <prefix>i:<namespace> indexes the live keys with
// score 0, so ZRANGEBYLEX gives the ordered, prefixed, cursor-able iteration the
// Scan contract needs, and ZCARD backs the item quota.
//
// Expiry is authoritative on the ex field (read paths filter ex > now, matching
// the Postgres driver's `expires_at > now()`), with a native PEXPIRE on the hash
// as a memory-reclamation backstop. When a hash is reclaimed its index member is
// pruned lazily — on Scan, on Delete, and (to keep the quota honest) during a
// Put that finds itself at the cap.
//
// Writes that must be atomic against concurrent callers (Put's version bump +
// CAS + quota, Delete's CAS) run as single Lua scripts, which Redis executes
// atomically.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"memsidecar/internal/kv"
)

// recordSep joins namespace and key inside a hash key. It never appears in a
// namespace or key produced by the service layer's namespace qualification
// (which uses \x1f), and callers address records by (namespace, key) pairs, so
// the join is unambiguous per namespace.
const recordSep = "\x1e"

// DefaultKeyPrefix namespaces every key this driver writes, so a Redis instance
// can be shared with other tenants of the same server.
const DefaultKeyPrefix = "memsidecar:"

// Options configures a Driver.
type Options struct {
	// DSN is a redis:// (or rediss://) URL, e.g.
	// redis://:pass@host:6379/0. Required unless Client is supplied.
	DSN string
	// Client, when set, is used as-is and DSN is ignored (useful for tests and
	// for sharing a pool). The driver takes ownership and closes it on Close.
	Client *redis.Client
	// KeyPrefix overrides DefaultKeyPrefix. Empty uses the default.
	KeyPrefix string
	// DialTimeout bounds the initial connectivity check. 0 → 5s.
	DialTimeout time.Duration
}

// Driver is a Redis/Valkey-backed kv.Driver. Safe for concurrent use.
type Driver struct {
	c      *redis.Client
	prefix string
	now    func() time.Time
}

// New connects to Redis/Valkey and verifies connectivity.
func New(ctx context.Context, opts Options) (*Driver, error) {
	c := opts.Client
	if c == nil {
		if opts.DSN == "" {
			return nil, errors.New("redis: dsn (or Client) required")
		}
		ro, err := redis.ParseURL(opts.DSN)
		if err != nil {
			return nil, fmt.Errorf("redis: parse dsn: %w", err)
		}
		c = redis.NewClient(ro)
	}
	prefix := opts.KeyPrefix
	if prefix == "" {
		prefix = DefaultKeyPrefix
	}
	dt := opts.DialTimeout
	if dt <= 0 {
		dt = 5 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(ctx, dt)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("redis: ping: %w", err)
	}
	return &Driver{c: c, prefix: prefix, now: time.Now}, nil
}

func (d *Driver) Close() error { return d.c.Close() }

func (d *Driver) hashKey(namespace, key string) string {
	return d.prefix + "h:" + namespace + recordSep + key
}

func (d *Driver) hashPrefix(namespace string) string {
	return d.prefix + "h:" + namespace + recordSep
}

func (d *Driver) idxKey(namespace string) string {
	return d.prefix + "i:" + namespace
}

// putScript atomically upserts a record: CAS on version (ARGV[6] >= 0), item
// quota on a new key (ARGV[7] > 0, self-healing a stale index at the cap), a
// version bump, the field write, TTL (native PEXPIRE backstop), and the index
// add. KEYS: hash, index. ARGV: key, value, ct, md, ttlMs, ifVersion,
// maxItems, nowMs, hashPrefix. Returns {version, createdMs, expiresMs} (all
// strings) or a "KVVERSION"/"KVQUOTA" error.
var putScript = redis.NewScript(`
local exists = redis.call('EXISTS', KEYS[1])
local curVer = 0
local created = ARGV[8]
if exists == 1 then
  curVer = tonumber(redis.call('HGET', KEYS[1], 'ver'))
  created = redis.call('HGET', KEYS[1], 'cr')
end
local ifv = tonumber(ARGV[6])
if ifv >= 0 then
  local got = 0
  if exists == 1 then got = curVer end
  if ifv ~= got then return redis.error_reply('KVVERSION') end
end
local maxItems = tonumber(ARGV[7])
if exists == 0 and maxItems > 0 then
  local cnt = redis.call('ZCARD', KEYS[2])
  if cnt >= maxItems then
    -- Prune index members whose hash was reclaimed, then re-check.
    local members = redis.call('ZRANGE', KEYS[2], 0, -1)
    local live = 0
    for i = 1, #members do
      if redis.call('EXISTS', ARGV[9] .. members[i]) == 1 then
        live = live + 1
      else
        redis.call('ZREM', KEYS[2], members[i])
      end
    end
    if live >= maxItems then return redis.error_reply('KVQUOTA') end
  end
end
local ver = curVer + 1
local ttlMs = tonumber(ARGV[5])
local ex = '0'
redis.call('HSET', KEYS[1], 'v', ARGV[2], 'ct', ARGV[3], 'md', ARGV[4], 'ver', ver, 'cr', created)
if ttlMs > 0 then
  ex = tostring(tonumber(ARGV[8]) + ttlMs)
  redis.call('HSET', KEYS[1], 'ex', ex)
  redis.call('PEXPIRE', KEYS[1], ttlMs)
else
  redis.call('HSET', KEYS[1], 'ex', '0')
  redis.call('PERSIST', KEYS[1])
end
redis.call('ZADD', KEYS[2], 0, ARGV[1])
return {tostring(ver), created, ex}
`)

// delScript atomically deletes a record, treating a logically-expired hash as
// absent and honouring an optional CAS (ARGV[2] >= 0). KEYS: hash, index.
// ARGV: key, ifVersion, nowMs. Returns 1 if a live record was removed, else 0,
// or a "KVVERSION" error.
var delScript = redis.NewScript(`
local exists = redis.call('EXISTS', KEYS[1])
local live = 0
local curVer = 0
if exists == 1 then
  local ex = tonumber(redis.call('HGET', KEYS[1], 'ex'))
  if ex == 0 or ex > tonumber(ARGV[3]) then
    live = 1
    curVer = tonumber(redis.call('HGET', KEYS[1], 'ver'))
  end
end
local ifv = tonumber(ARGV[2])
if ifv >= 0 then
  local got = 0
  if live == 1 then got = curVer end
  if ifv ~= got then return redis.error_reply('KVVERSION') end
end
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[1])
return live
`)

func (d *Driver) Put(ctx context.Context, namespace, key string, opts kv.PutOptions) (kv.Record, error) {
	metaBytes, err := json.Marshal(opts.Metadata)
	if err != nil {
		return kv.Record{}, fmt.Errorf("redis: marshal metadata: %w", err)
	}
	nowMs := d.now().UnixMilli()
	ttlMs := int64(0)
	if opts.TTL > 0 {
		ttlMs = opts.TTL.Milliseconds()
		if ttlMs <= 0 {
			ttlMs = 1 // sub-millisecond TTL still expires
		}
	}
	ifVersion := int64(-1)
	if opts.IfVersion != nil {
		ifVersion = int64(*opts.IfVersion)
	}

	res, err := putScript.Run(ctx, d.c,
		[]string{d.hashKey(namespace, key), d.idxKey(namespace)},
		key, opts.Value, opts.ContentType, string(metaBytes),
		ttlMs, ifVersion, opts.MaxItems, nowMs, d.hashPrefix(namespace),
	).Result()
	if err != nil {
		// Redis prefixes a single-word Lua error_reply with "ERR ", so match on
		// the sentinel token rather than the whole message.
		switch {
		case strings.Contains(err.Error(), "KVVERSION"):
			return kv.Record{}, kv.ErrVersionMismatch
		case strings.Contains(err.Error(), "KVQUOTA"):
			return kv.Record{}, kv.ErrQuotaExceeded
		}
		return kv.Record{}, fmt.Errorf("redis: put: %w", err)
	}

	out, _ := res.([]interface{})
	if len(out) != 3 {
		return kv.Record{}, fmt.Errorf("redis: put: unexpected reply %v", res)
	}
	ver, _ := strconv.ParseUint(fmt.Sprint(out[0]), 10, 64)
	createdMs, _ := strconv.ParseInt(fmt.Sprint(out[1]), 10, 64)
	expiresMs, _ := strconv.ParseInt(fmt.Sprint(out[2]), 10, 64)
	rec := kv.Record{
		Key:         key,
		Value:       opts.Value,
		ContentType: opts.ContentType,
		Metadata:    opts.Metadata,
		Version:     ver,
		CreatedAt:   time.UnixMilli(createdMs),
	}
	if expiresMs > 0 {
		rec.ExpiresAt = time.UnixMilli(expiresMs)
	}
	return rec, nil
}

func (d *Driver) Delete(ctx context.Context, namespace, key string, opts kv.DeleteOptions) (bool, error) {
	ifVersion := int64(-1)
	if opts.IfVersion != nil {
		ifVersion = int64(*opts.IfVersion)
	}
	res, err := delScript.Run(ctx, d.c,
		[]string{d.hashKey(namespace, key), d.idxKey(namespace)},
		key, ifVersion, d.now().UnixMilli(),
	).Result()
	if err != nil {
		if strings.Contains(err.Error(), "KVVERSION") {
			return false, kv.ErrVersionMismatch
		}
		return false, fmt.Errorf("redis: delete: %w", err)
	}
	n, _ := res.(int64)
	return n == 1, nil
}

func (d *Driver) Get(ctx context.Context, namespace, key string) (kv.Record, error) {
	m, err := d.c.HGetAll(ctx, d.hashKey(namespace, key)).Result()
	if err != nil {
		return kv.Record{}, fmt.Errorf("redis: get: %w", err)
	}
	rec, live := d.parse(key, m)
	if !live {
		// Present-but-expired: reclaim eagerly (mirrors the memory driver's lazy
		// expiry). A truly-absent hash yields an empty map and no-op deletes.
		if len(m) > 0 {
			d.c.Del(ctx, d.hashKey(namespace, key))
			d.c.ZRem(ctx, d.idxKey(namespace), key)
		}
		return kv.Record{}, kv.ErrNotFound
	}
	return rec, nil
}

func (d *Driver) MultiGet(ctx context.Context, namespace string, keys []string) ([]kv.Record, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(keys))
	uniq := keys[:0:0]
	for _, k := range keys {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
	}

	pipe := d.c.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(uniq))
	for i, k := range uniq {
		cmds[i] = pipe.HGetAll(ctx, d.hashKey(namespace, k))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("redis: multiget: %w", err)
	}

	var out []kv.Record
	for i, cmd := range cmds {
		m, err := cmd.Result()
		if err != nil {
			return nil, fmt.Errorf("redis: multiget: %w", err)
		}
		if rec, live := d.parse(uniq[i], m); live {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// scanBatch bounds how many records Scan fetches per round-trip.
const scanBatch = 256

func (d *Driver) Scan(ctx context.Context, namespace string, opts kv.ScanOptions, yield func(kv.Record) error) error {
	lo, hi := lexRange(opts.KeyPrefix, opts.StartAfter)
	candidates, err := d.c.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key: d.idxKey(namespace), ByLex: true, Start: lo, Stop: hi,
	}).Result()
	if err != nil {
		return fmt.Errorf("redis: scan: %w", err)
	}

	var stale []string
	defer func() {
		if len(stale) > 0 {
			d.c.ZRem(context.WithoutCancel(ctx), d.idxKey(namespace), stringsToAny(stale)...)
		}
	}()

	emitted := uint32(0)
	for start := 0; start < len(candidates); start += scanBatch {
		batch := candidates[start:min(start+scanBatch, len(candidates))]

		pipe := d.c.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, len(batch))
		for i, k := range batch {
			cmds[i] = pipe.HGetAll(ctx, d.hashKey(namespace, k))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("redis: scan: %w", err)
		}
		for i, cmd := range cmds {
			m, err := cmd.Result()
			if err != nil {
				return fmt.Errorf("redis: scan: %w", err)
			}
			rec, live := d.parse(batch[i], m)
			if !live {
				stale = append(stale, batch[i]) // hash reclaimed; drop the index member
				continue
			}
			if !opts.IncludeValues {
				rec.Value = nil
			}
			if err := yield(rec); err != nil {
				return err
			}
			emitted++
			if opts.Limit > 0 && emitted >= opts.Limit {
				return nil
			}
		}
	}
	return nil
}

// parse builds a Record from a hash's fields, reporting whether it is live
// (present and not past its expiry).
func (d *Driver) parse(key string, m map[string]string) (kv.Record, bool) {
	if len(m) == 0 {
		return kv.Record{}, false
	}
	exMs, _ := strconv.ParseInt(m["ex"], 10, 64)
	if exMs > 0 && exMs <= d.now().UnixMilli() {
		return kv.Record{}, false
	}
	ver, _ := strconv.ParseUint(m["ver"], 10, 64)
	createdMs, _ := strconv.ParseInt(m["cr"], 10, 64)
	rec := kv.Record{
		Key:         key,
		Value:       []byte(m["v"]),
		ContentType: m["ct"],
		Version:     ver,
		CreatedAt:   time.UnixMilli(createdMs),
	}
	if exMs > 0 {
		rec.ExpiresAt = time.UnixMilli(exMs)
	}
	if md := m["md"]; md != "" && md != "{}" {
		_ = json.Unmarshal([]byte(md), &rec.Metadata)
	}
	return rec, true
}

// lexRange builds ZRANGEBYLEX bounds (lo, hi) for a prefix scan resuming after
// startAfter (exclusive). Byte ordering matches the memory driver's
// sort.Strings and the Postgres driver's COLLATE "C".
func lexRange(prefix, startAfter string) (lo, hi string) {
	if prefix == "" {
		hi = "+"
	} else if succ, ok := lexSuccessor(prefix); ok {
		hi = "(" + succ
	} else {
		hi = "+"
	}
	if startAfter != "" && startAfter >= prefix {
		lo = "(" + startAfter // exclusive cursor
	} else {
		lo = "[" + prefix // inclusive prefix start
	}
	return lo, hi
}

// lexSuccessor returns the shortest string strictly greater than every string
// with prefix s (s with its last non-0xFF byte incremented). ok is false when s
// is all 0xFF bytes, i.e. there is no finite upper bound.
func lexSuccessor(s string) (string, bool) {
	b := []byte(s)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

func stringsToAny(s []string) []interface{} {
	out := make([]interface{}, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
