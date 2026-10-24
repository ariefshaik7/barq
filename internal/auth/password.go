package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PBKDF2-HMAC-SHA256 parameters.
//
// The iteration count follows the OWASP password storage cheat sheet.
// Each iteration is a HMAC over a 32 byte block, so 600000 iterations
// is roughly a quarter of a second on a modern core. That cost is the
// entire point: it is what makes an offline cracking attack against a
// leaked hash database expensive.
//
// PBKDF2 is used rather than Argon2id because the Go standard library
// provides no memory-hard KDF, and Barq's zero external dependency
// constraint is a hard requirement. Adding golang.org/x/crypto for
// Argon2id would be a defensible trade in most projects, but it would
// break the "one binary, no module graph" property that this project
// is built around.
const (
	pbkdf2Iterations = 600000
	pbkdf2KeyLength  = 32
	pbkdf2SaltLength = 16
)

// dummyHash is used to equalise the timing of a login attempt against
// an account that does not exist.
var dummyHash = encodeHash(randomSalt(), pbkdf2Key([]byte("timing-equalisation-placeholder"), randomSalt(), pbkdf2Iterations, pbkdf2KeyLength))

// ValidatePassword enforces the password policy.
//
// The policy is length based rather than composition based. Length is
// the property that actually matters, and composition rules push users
// towards predictable substitutions.
func ValidatePassword(password string) error {
	if len(password) < 12 {
		return fmt.Errorf("%w: at least 12 characters are required", ErrWeakPassword)
	}
	if len(password) > 256 {
		return fmt.Errorf("%w: at most 256 characters are allowed", ErrWeakPassword)
	}
	var hasLetter, hasOther bool
	for _, char := range password {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z':
			hasLetter = true
		default:
			hasOther = true
		}
	}
	if !hasLetter || !hasOther {
		return fmt.Errorf("%w: the password must mix letters with digits or symbols", ErrWeakPassword)
	}
	return nil
}

// HashPassword produces an encoded PBKDF2-HMAC-SHA256 hash.
func HashPassword(password string) (string, error) {
	salt := randomSalt()
	key := pbkdf2Key([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLength)
	return encodeHash(salt, key), nil
}

// VerifyPassword checks a password against an encoded hash.
func VerifyPassword(password, encoded string) bool {
	salt, key, iterations, err := decodeHash(encoded)
	if err != nil {
		return false
	}
	candidate := pbkdf2Key([]byte(password), salt, iterations, len(key))
	return subtle.ConstantTimeCompare(candidate, key) == 1
}

// randomSalt produces a fresh salt.
func randomSalt() []byte {
	salt := make([]byte, pbkdf2SaltLength)
	if _, err := rand.Read(salt); err != nil {
		// crypto/rand does not fail in practice. If it does, the
		// process is already in an unrecoverable state.
		panic("auth: unable to read random bytes: " + err.Error())
	}
	return salt
}

// pbkdf2Key implements PBKDF2 with HMAC-SHA256 as the pseudorandom
// function, as specified in RFC 8018 section 5.2.
func pbkdf2Key(password, salt []byte, iterations, keyLength int) []byte {
	hashLength := sha256.Size
	blocks := (keyLength + hashLength - 1) / hashLength
	output := make([]byte, 0, blocks*hashLength)

	for block := 1; block <= blocks; block++ {
		// U1 = PRF(P, S || INT_32_BE(i))
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		var counter [4]byte
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		mac.Write(counter[:])
		u := mac.Sum(nil)

		result := make([]byte, hashLength)
		copy(result, u)

		// T(i) = U1 xor U2 xor ... xor Uc
		for iteration := 1; iteration < iterations; iteration++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for index := range result {
				result[index] ^= u[index]
			}
		}
		output = append(output, result...)
	}
	return output[:keyLength]
}

// encodeHash renders a salt and key in a self describing format.
func encodeHash(salt, key []byte) string {
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// decodeHash parses an encoded hash.
func decodeHash(encoded string) (salt, key []byte, iterations int, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return nil, nil, 0, errors.New("auth: malformed password hash")
	}
	iterations, err = strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return nil, nil, 0, errors.New("auth: malformed password hash")
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, 0, err
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, nil, 0, err
	}
	return salt, key, iterations, nil
}
