package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoad_Valid(t *testing.T) {
	cfg, err := Load(writeTemp(t, `
server:
  grpc:
    tcp: "127.0.0.1:7777"
    uds: "/tmp/mindd.sock"

auth:
  verifier: paseto
  paseto:
    public_key_hex: "deadbeef"

backends:
  - name: mem-default
    driver: memory

namespaces:
  - block: kv
    name: scratchpad
    backend: mem-default
`))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7777", cfg.Server.GRPC.TCP)
	assert.Equal(t, "/tmp/mindd.sock", cfg.Server.GRPC.UDS)
	require.Len(t, cfg.Backends, 1)
	require.Len(t, cfg.Namespaces, 1)
	assert.Equal(t, "scratchpad", cfg.Namespaces[0].Name)
}

func TestLoad_UnknownBackend(t *testing.T) {
	_, err := Load(writeTemp(t, `
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
namespaces:
  - { block: kv, name: x, backend: missing }
`))
	require.ErrorContains(t, err, "unknown backend")
}

func TestLoad_UnknownDriver(t *testing.T) {
	_, err := Load(writeTemp(t, `
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: redis }
`))
	require.ErrorContains(t, err, "unknown driver")
}

func TestLoad_UnknownBlock(t *testing.T) {
	_, err := Load(writeTemp(t, `
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
namespaces:
  - { block: zzznew, name: x, backend: mem }
`))
	require.ErrorContains(t, err, "unsupported block")
}

func TestLoad_EpisodicBlock(t *testing.T) {
	cfg, err := Load(writeTemp(t, `
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
namespaces:
  - { block: kv, name: scratchpad, backend: mem }
  - { block: episodic, name: events, backend: mem }
`))
	require.NoError(t, err)
	require.Len(t, cfg.Namespaces, 2)
	assert.Equal(t, "episodic", cfg.Namespaces[1].Block)
}

func TestLoad_SemanticBlock(t *testing.T) {
	cfg, err := Load(writeTemp(t, `
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
namespaces:
  - block: semantic
    name: notes
    backend: mem
    embedder:
      provider: fake
      model: fake-64
      dimensions: 64
`))
	require.NoError(t, err)
	require.Len(t, cfg.Namespaces, 1)
	assert.Equal(t, "fake", cfg.Namespaces[0].Embedder.Provider)
	assert.Equal(t, 64, cfg.Namespaces[0].Embedder.Dimensions)
}

func TestLoad_SemanticMissingEmbedder(t *testing.T) {
	_, err := Load(writeTemp(t, `
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
namespaces:
  - { block: semantic, name: notes, backend: mem }
`))
	require.ErrorContains(t, err, "embedder.provider required")
}

func TestLoad_SemanticZeroDimensions(t *testing.T) {
	_, err := Load(writeTemp(t, `
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
namespaces:
  - block: semantic
    name: notes
    backend: mem
    embedder: { provider: fake, dimensions: 0 }
`))
	require.ErrorContains(t, err, "embedder.dimensions must be > 0")
}

func TestLoad_MissingPASETOKey(t *testing.T) {
	_, err := Load(writeTemp(t, `
auth:
  verifier: paseto
backends:
  - { name: mem, driver: memory }
`))
	require.ErrorContains(t, err, "public_key_hex")
}

func TestLoad_DuplicateBackend(t *testing.T) {
	_, err := Load(writeTemp(t, `
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
  - { name: mem, driver: memory }
`))
	require.ErrorContains(t, err, "duplicate backend")
}

func TestLoad_TenantIsolationWithSemantic(t *testing.T) {
	// Semantic isolation is implemented, so this combination is now valid.
	cfg, err := Load(writeTemp(t, `
tenant_isolation: true
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
namespaces:
  - block: semantic
    name: notes
    backend: mem
    embedder: { provider: fake, model: fake-64, dimensions: 64 }
`))
	require.NoError(t, err)
	assert.True(t, cfg.TenantIsolation)
}

func TestLoad_TenantIsolationOK(t *testing.T) {
	cfg, err := Load(writeTemp(t, `
tenant_isolation: true
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
namespaces:
  - { block: kv, name: scratchpad, backend: mem }
  - { block: episodic, name: events, backend: mem }
`))
	require.NoError(t, err)
	assert.True(t, cfg.TenantIsolation)
}

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("MINDD_SERVER__GRPC__TCP", "0.0.0.0:9999")
	cfg, err := Load(writeTemp(t, `
auth:
  verifier: paseto
  paseto: { public_key_hex: dead }
backends:
  - { name: mem, driver: memory }
`))
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:9999", cfg.Server.GRPC.TCP)
}

const encPreamble = `
server:
  grpc:
    tcp: "127.0.0.1:7777"

auth:
  verifier: paseto
  paseto:
    public_key_hex: "deadbeef"

backends:
  - name: mem-default
    driver: memory
`

func TestLoad_EncryptionOK(t *testing.T) {
	cfg, err := Load(writeTemp(t, encPreamble+`
encryption:
  allow_plaintext_reads: true
  keys:
    - id: k2
      secret_env: MINDD_ENC_K2
    - id: k1
      secret_env: MINDD_ENC_K1

namespaces:
  - block: kv
    name: secrets
    backend: mem-default
    encrypt: true
  - block: kv
    name: tool-cache
    backend: mem-default
  - block: episodic
    name: events
    backend: mem-default
    encrypt: true
`))
	require.NoError(t, err)
	require.True(t, cfg.Encryption.Enabled())
	assert.True(t, cfg.Encryption.AllowPlaintextReads)
	require.Len(t, cfg.Encryption.Keys, 2)
	// Order is load-bearing: the first key is the active one.
	assert.Equal(t, "k2", cfg.Encryption.Keys[0].ID)
	assert.Equal(t, "MINDD_ENC_K2", cfg.Encryption.Keys[0].SecretEnv)

	assert.True(t, cfg.Namespaces[0].Encrypt)
	assert.False(t, cfg.Namespaces[1].Encrypt, "encrypt must default off")
	assert.True(t, cfg.Namespaces[2].Encrypt)
}

func TestLoad_EncryptionDefaultsOff(t *testing.T) {
	cfg, err := Load(writeTemp(t, encPreamble+`
namespaces:
  - block: kv
    name: scratchpad
    backend: mem-default
`))
	require.NoError(t, err)
	assert.False(t, cfg.Encryption.Enabled())
	assert.False(t, cfg.Namespaces[0].Encrypt)
}

// Marking a namespace encrypted without a keyring must fail loudly rather than
// booting and silently storing plaintext.
func TestLoad_EncryptWithoutKeys(t *testing.T) {
	_, err := Load(writeTemp(t, encPreamble+`
namespaces:
  - block: kv
    name: secrets
    backend: mem-default
    encrypt: true
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires encryption.keys")
}

// Blocks without an encrypting decorator must be rejected, not silently ignored.
func TestLoad_EncryptUnsupportedBlock(t *testing.T) {
	for _, block := range []string{"artifact", "lease", "graph"} {
		t.Run(block, func(t *testing.T) {
			_, err := Load(writeTemp(t, encPreamble+`
encryption:
  keys:
    - id: k1
      secret_env: MINDD_ENC_K1

namespaces:
  - block: `+block+`
    name: things
    backend: mem-default
    encrypt: true
`))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not supported for block")
		})
	}
}

func TestLoad_EncryptionKeyValidation(t *testing.T) {
	tests := []struct {
		name string
		keys string
		want string
	}{
		{
			"missing id",
			"    - secret_env: MINDD_ENC_K1\n",
			"id required",
		},
		{
			"missing secret_env",
			"    - id: k1\n",
			"secret_env required",
		},
		{
			"duplicate id",
			"    - id: k1\n      secret_env: A\n    - id: k1\n      secret_env: B\n",
			"duplicate id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, encPreamble+"\nencryption:\n  keys:\n"+tt.keys+`
namespaces:
  - block: kv
    name: scratchpad
    backend: mem-default
`))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// allow_plaintext_reads on its own is almost certainly a mistake — it only has
// meaning alongside a keyring.
func TestLoad_AllowPlaintextReadsWithoutKeys(t *testing.T) {
	_, err := Load(writeTemp(t, encPreamble+`
encryption:
  allow_plaintext_reads: true

namespaces:
  - block: kv
    name: scratchpad
    backend: mem-default
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_plaintext_reads")
}
