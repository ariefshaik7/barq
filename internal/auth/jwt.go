package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
)

// jsonWebKey is a single entry of a JSON web key set.
type jsonWebKey struct {
	KeyType string `json:"kty"`
	KeyID   string `json:"kid"`
	Use     string `json:"use"`
	Alg     string `json:"alg"`

	// RSA fields.
	N string `json:"n"`
	E string `json:"e"`

	// Elliptic curve fields.
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// verifySignature validates a JWS signature against a key set.
//
// The algorithm is supplied by the caller and is pinned, so a token
// that advertises "none", or a symmetric algorithm, is rejected before
// any key lookup happens.
func verifySignature(signingInput string, signature []byte, keys []jsonWebKey, keyID, algorithm string) error {
	for _, key := range keys {
		if key.KeyID != "" && keyID != "" && key.KeyID != keyID {
			continue
		}
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if err := verifyWithKey(signingInput, signature, key, algorithm); err == nil {
			return nil
		}
	}
	return fmt.Errorf("auth: the id token signature is not valid")
}

// verifyWithKey validates a signature against a single key.
func verifyWithKey(signingInput string, signature []byte, key jsonWebKey, algorithm string) error {
	switch algorithm {
	case "RS256":
		publicKey, err := rsaPublicKey(key)
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(signingInput))
		return rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, sum[:], signature)
	case "ES256":
		publicKey, err := ecdsaPublicKey(key)
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(signingInput))
		// A P-256 signature is the raw R||S pair.
		if len(signature) != 64 {
			return fmt.Errorf("auth: malformed ECDSA signature")
		}
		r := new(big.Int).SetBytes(signature[:32])
		s := new(big.Int).SetBytes(signature[32:])
		if !ecdsa.Verify(publicKey, sum[:], r, s) {
			return fmt.Errorf("auth: ECDSA verification failed")
		}
		return nil
	case "RS384":
		publicKey, err := rsaPublicKey(key)
		if err != nil {
			return err
		}
		sum := sha512.Sum384([]byte(signingInput))
		return rsa.VerifyPKCS1v15(publicKey, crypto.SHA384, sum[:], signature)
	case "RS512":
		publicKey, err := rsaPublicKey(key)
		if err != nil {
			return err
		}
		sum := sha512.Sum512([]byte(signingInput))
		return rsa.VerifyPKCS1v15(publicKey, crypto.SHA512, sum[:], signature)
	default:
		return fmt.Errorf("auth: unsupported signature algorithm %q", algorithm)
	}
}

// rsaPublicKey reconstructs an RSA public key from a JWK.
func rsaPublicKey(key jsonWebKey) (*rsa.PublicKey, error) {
	if key.N == "" || key.E == "" {
		return nil, fmt.Errorf("auth: the JWK is not an RSA key")
	}
	modulus, err := base64.RawURLEncoding.DecodeString(key.N)
	if err != nil {
		return nil, err
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(key.E)
	if err != nil {
		return nil, err
	}
	exponent := 0
	for _, value := range exponentBytes {
		exponent = exponent<<8 | int(value)
	}
	if exponent == 0 {
		return nil, fmt.Errorf("auth: the JWK has an invalid RSA exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponent}, nil
}

// ecdsaPublicKey reconstructs an elliptic curve public key from a JWK.
func ecdsaPublicKey(key jsonWebKey) (*ecdsa.PublicKey, error) {
	if key.Crv != "P-256" {
		return nil, fmt.Errorf("auth: unsupported elliptic curve %q", key.Crv)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(key.X)
	if err != nil {
		return nil, err
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(key.Y)
	if err != nil {
		return nil, err
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}, nil
}

// sha256Sum is a small helper so the OIDC file does not need to import
// crypto/sha256 directly.
func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// decodeJSONWebKeySet parses a key set document. It exists for tests
// and for administrative tooling.
func decodeJSONWebKeySet(data []byte) ([]jsonWebKey, error) {
	var document struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	return document.Keys, nil
}
