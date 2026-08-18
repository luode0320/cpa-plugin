package usagestats

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

)

const (
	apiKeyCipherVersionV1 byte = 1
	apiKeyCipherVersionV2 byte = 2
	apiKeyCipherVersion        = apiKeyCipherVersionV1
	defaultAPIKeySecret        = "123456"
	apiKeyHashVersion          = "hmac-sha256-128-v1"
)

type cryptoContext struct {
	encKey            [32]byte
	indexKey          [32]byte
	keyID             string
	enabled           bool
	usesDefaultSecret bool
}

type APIKeyCryptoGeneration struct {
	ID              uint64    `json:"id"`
	KeyID           string    `json:"key_id,omitempty"`
	HashVersion     string    `json:"hash_version,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	IdentityMissing bool      `json:"identity_missing,omitempty"`
}

const (
	apiKeyStatusAvailable             = "available"
	apiKeyStatusGenerationUnavailable = "generation_unavailable"
	apiKeyStatusCiphertextMissing     = "ciphertext_missing"
	apiKeyStatusCiphertextInvalid     = "ciphertext_invalid"
	apiKeyStatusIdentityMissing       = "identity_missing"
	apiKeyStatusSourceMissing         = "source_missing"
)

type DecryptFunc func(ciphertext, fingerprint string, generation uint64) (string, string)

type Sensitive interface {
	Redact()
	Reveal(decrypt DecryptFunc)
}

func deriveDomainKey(secret, domain string) [32]byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("cap-token-usage-tracker/" + domain + "/v1"))
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func deriveCryptoContext(secret string) (cryptoContext, error) {
	if secret == "" {
		return cryptoContext{}, nil
	}
	if secret != defaultAPIKeySecret && len([]byte(secret)) < 32 {
		return cryptoContext{}, errors.New("api_key_secret must be empty, 123456, or at least 32 bytes")
	}
	ctx := cryptoContext{
		encKey:            deriveDomainKey(secret, "api-key-encryption"),
		indexKey:          deriveDomainKey(secret, "api-key-index"),
		enabled:           true,
		usesDefaultSecret: secret == defaultAPIKeySecret,
	}
	idInput := append([]byte("cap-token-usage-tracker/key-id/v1:"), ctx.indexKey[:]...)
	id := sha256.Sum256(idInput)
	ctx.keyID = hex.EncodeToString(id[:16])
	return ctx, nil
}

func apiKeyFingerprint(apiKey string, indexKey [32]byte) string {
	if apiKey == "" {
		return ""
	}
	mac := hmac.New(sha256.New, indexKey[:])
	_, _ = mac.Write([]byte(apiKey))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func encryptAPIKey(ctx cryptoContext, plaintext, fingerprint string) (string, error) {
	return encryptAPIKeyVersion(ctx, plaintext, fingerprint, 0, apiKeyCipherVersionV1)
}

func encryptAPIKeyForGeneration(ctx cryptoContext, plaintext, fingerprint string, generation uint64) (string, error) {
	if generation == 0 {
		return "", errors.New("api key generation is required")
	}
	return encryptAPIKeyVersion(ctx, plaintext, fingerprint, generation, apiKeyCipherVersionV2)
}

func encryptAPIKeyVersion(ctx cryptoContext, plaintext, fingerprint string, generation uint64, version byte) (string, error) {
	if !ctx.enabled || plaintext == "" || fingerprint == "" {
		return "", nil
	}
	block, err := aes.NewCipher(ctx.encKey[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	aad, err := apiKeyAAD(version, generation, fingerprint)
	if err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), aad)
	combined := make([]byte, 1, 1+len(nonce)+len(sealed))
	combined[0] = version
	combined = append(combined, nonce...)
	combined = append(combined, sealed...)
	return base64.RawURLEncoding.EncodeToString(combined), nil
}

func decryptAPIKey(ctx cryptoContext, ciphertext, fingerprint string) (string, error) {
	return decryptAPIKeyForGeneration(ctx, ciphertext, fingerprint, 0)
}

func decryptAPIKeyForGeneration(ctx cryptoContext, ciphertext, fingerprint string, generation uint64) (string, error) {
	if !ctx.enabled || ciphertext == "" || fingerprint == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	if len(raw) < 1 {
		return "", errors.New("unsupported api key ciphertext version")
	}
	version := raw[0]
	raw = raw[1:]
	block, err := aes.NewCipher(ctx.encKey[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize+gcm.Overhead() {
		return "", errors.New("ciphertext too short")
	}
	aad, err := apiKeyAAD(version, generation, fingerprint)
	if err != nil {
		return "", err
	}
	plaintext, err := gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], aad)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func apiKeyAAD(version byte, generation uint64, fingerprint string) ([]byte, error) {
	switch version {
	case apiKeyCipherVersionV1:
		return []byte("api-key/v1:" + fingerprint), nil
	case apiKeyCipherVersionV2:
		if generation == 0 {
			return nil, errors.New("api key generation is required for ciphertext v2")
		}
		return []byte(fmt.Sprintf("api-key/v2:%d:%s", generation, fingerprint)), nil
	default:
		return nil, errors.New("unsupported api key ciphertext version")
	}
}

func apiKeyRef(generation uint64, fingerprint string) string {
	if generation == 0 || !validAPIKeyHash(fingerprint) {
		return ""
	}
	return "g" + strconv.FormatUint(generation, 10) + ":" + fingerprint
}

func parseAPIKeyRef(ref string) (uint64, string, bool) {
	prefix, hash, ok := strings.Cut(ref, ":")
	if !ok || len(prefix) < 2 || prefix[0] != 'g' || !validAPIKeyHash(hash) {
		return 0, "", false
	}
	generation, err := strconv.ParseUint(prefix[1:], 10, 64)
	if err != nil || generation == 0 || apiKeyRef(generation, hash) != ref {
		return 0, "", false
	}
	return generation, hash, true
}
