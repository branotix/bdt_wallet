package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// PINs are low-entropy credentials, so use a memory-hard password KDF. The
// encoded format is self-contained and leaves the old SHA-256 based format
// verifiable so existing accounts can migrate transparently on next login.
const (
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonTime    uint32 = 3
	argonThreads uint8  = 2
	argonKeyLen  uint32 = 32
)

func HashPIN(pin string) (hash string, salt string, err error) {
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", "", err
	}
	derived := argon2.IDKey([]byte(pin), saltBytes, argonTime, argonMemory, argonThreads, argonKeyLen)
	encoded := base64.RawStdEncoding.EncodeToString(derived)
	saltEncoded := base64.RawStdEncoding.EncodeToString(saltBytes)
	// Keep the salt column populated for schema compatibility, while the hash
	// contains the algorithm parameters so future upgrades can coexist.
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, saltEncoded, encoded), saltEncoded, nil
}

func NeedsRehash(storedHash string) bool { return !strings.HasPrefix(storedHash, "argon2id$") }

func VerifyPIN(pin, storedHash, storedSalt string) bool {
	if strings.HasPrefix(storedHash, "argon2id$") {
		parts := strings.Split(storedHash, "$")
		if len(parts) != 5 || parts[1] != "v=19" {
			return false
		}
		var m, t uint32
		var p uint32
		if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || t == 0 || p == 0 || p > 255 || m > 1024*1024 {
			return false
		}
		salt, err := base64.RawStdEncoding.DecodeString(parts[3])
		if err != nil || len(salt) < 16 {
			return false
		}
		want, err := base64.RawStdEncoding.DecodeString(parts[4])
		if err != nil || len(want) != int(argonKeyLen) {
			return false
		}
		got := argon2.IDKey([]byte(pin), salt, t, m, uint8(p), uint32(len(want)))
		return subtle.ConstantTimeCompare(got, want) == 1
	}

	// Legacy format: SHA-256 with a random salt and 200k iterations. This is
	// intentionally retained only for migration; successful login callers can
	// replace it with the Argon2id representation.
	if storedSalt == "" || storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(legacyDerive(pin, storedSalt)), []byte(storedHash)) == 1
}

func legacyDerive(pin, salt string) string {
	b := []byte(pin + ":" + salt)
	sum := sha256.Sum256(b)
	for i := 1; i < 200000; i++ {
		next := sha256.Sum256(append(sum[:], b...))
		sum = next
	}
	return hex.EncodeToString(sum[:])
}

func GenerateSessionToken() (plaintext, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	plaintext = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256(b)
	return plaintext, hex.EncodeToString(sum[:]), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
