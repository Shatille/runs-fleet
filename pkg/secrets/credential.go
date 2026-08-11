package secrets

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// credentialMaxDecodedBytes bounds what unpackCredential will inflate. A gzip
// stream can expand arbitrarily, and this value is read from a parameter store an
// agent trusts; the cap keeps a corrupt or hostile value from exhausting memory
// on a runner. Real credentials are ~3KB, so 1MB is orders of magnitude of slack.
const credentialMaxDecodedBytes = 1 << 20

// marshalConfigHalf renders the plaintext half of a RunnerConfig.
//
// It marshals through an alias with the two credential fields shadowed away
// rather than by zeroing them on a copy: RegistrationToken's `jit_token` tag
// carries no omitempty — deliberately, as a frozen wire contract — so a zeroed
// copy still emits an empty jit_token, putting a credential-shaped key in the
// plaintext parameter.
func marshalConfigHalf(config *RunnerConfig) ([]byte, error) {
	type configAlias RunnerConfig
	encoded, err := json.Marshal(&struct {
		*configAlias
		RegistrationToken *string `json:"jit_token,omitempty"`
		JITConfig         *string `json:"jit_config,omitempty"`
	}{configAlias: (*configAlias)(config)})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal runner config: %w", err)
	}

	return encoded, nil
}

// storedCredential is the credential parameter's payload. The JIT config and the
// registration token are mutually exclusive in practice, but both are carried so
// the parameter round-trips whichever the orchestrator minted.
type storedCredential struct {
	JITConfig         string `json:"jit_config,omitempty"`
	RegistrationToken string `json:"jit_token,omitempty"`
	// Compressed reports whether JITConfig holds the gzipped form written by
	// packCredential. Recorded rather than inferred so an uncompressed value —
	// written when compression did not pay off — reads back unambiguously.
	Compressed bool `json:"compressed,omitempty"`
}

// packCredential renders the credential half of a RunnerConfig.
//
// A GitHub JIT config is base64 of a JSON document whose bulk is a 2048-bit RSA
// private key, and at ~4100 characters it alone exceeds SSM's 4096-character
// Standard-tier ceiling. Decoding that outer base64 layer before gzipping is what
// makes it fit: compressing the base64 text directly saves ~8%, while compressing
// the bytes underneath saves ~29% — the difference between a few hundred
// characters of headroom and a thousand.
func packCredential(config *RunnerConfig) (string, error) {
	stored := storedCredential{RegistrationToken: config.RegistrationToken}

	if config.JITConfig != "" {
		packed, compressed, err := compressJITConfig(config.JITConfig)
		if err != nil {
			return "", err
		}
		stored.JITConfig = packed
		stored.Compressed = compressed
	}

	encoded, err := json.Marshal(&stored)
	if err != nil {
		return "", fmt.Errorf("failed to marshal credential: %w", err)
	}

	return string(encoded), nil
}

// compressJITConfig returns the storable form of a JIT config and whether it is
// compressed. Compression is skipped when it does not shrink the value, which
// keeps a non-base64 or already-dense credential from growing.
func compressJITConfig(jitConfig string) (string, bool, error) {
	raw, err := base64.StdEncoding.DecodeString(jitConfig)
	if err != nil {
		// Not base64: store as-is rather than fail. GitHub's format is not a
		// contract this package controls, and a credential that cannot be stored
		// means a job that never runs.
		return jitConfig, false, nil
	}

	var buf bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return "", false, fmt.Errorf("failed to create gzip writer: %w", err)
	}
	if _, err := writer.Write(raw); err != nil {
		return "", false, fmt.Errorf("failed to compress credential: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", false, fmt.Errorf("failed to finalize compressed credential: %w", err)
	}

	packed := base64.StdEncoding.EncodeToString(buf.Bytes())
	if len(packed) >= len(jitConfig) {
		return jitConfig, false, nil
	}

	return packed, true, nil
}

// unpackCredential restores a packed credential onto config.
// The credential fields are assigned only once every fallible step has passed,
// so a rejected credential leaves config untouched rather than half-applied —
// the caller then sees the error against the values it already had.
func unpackCredential(value string, config *RunnerConfig) error {
	var stored storedCredential
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return fmt.Errorf("failed to parse runner credential: %w", err)
	}

	if stored.JITConfig == "" {
		config.RegistrationToken = stored.RegistrationToken
		config.JITConfig = ""
		return nil
	}

	if !stored.Compressed {
		config.RegistrationToken = stored.RegistrationToken
		config.JITConfig = stored.JITConfig
		return nil
	}

	decoded, err := base64.StdEncoding.DecodeString(stored.JITConfig)
	if err != nil {
		return fmt.Errorf("failed to decode compressed credential: %w", err)
	}

	reader, err := gzip.NewReader(bytes.NewReader(decoded))
	if err != nil {
		return fmt.Errorf("failed to read compressed credential: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// One byte past the cap distinguishes "filled the cap" from "hit the cap and
	// had more to give", so an oversized credential fails here rather than
	// reaching the agent silently truncated — which would register a runner with
	// a mangled config instead of reporting a corrupt one.
	raw, err := io.ReadAll(io.LimitReader(reader, credentialMaxDecodedBytes+1))
	if err != nil {
		return fmt.Errorf("failed to decompress credential: %w", err)
	}
	if len(raw) > credentialMaxDecodedBytes {
		return fmt.Errorf("credential decompresses to more than %d bytes", credentialMaxDecodedBytes)
	}

	config.RegistrationToken = stored.RegistrationToken
	config.JITConfig = base64.StdEncoding.EncodeToString(raw)

	return nil
}
