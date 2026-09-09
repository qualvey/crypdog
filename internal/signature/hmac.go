package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// GenerateHMACSHA256 calculates the HMAC-SHA256 signature of a payload using a secret key
func GenerateHMACSHA256(payload []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyHMACSHA256 checks if the provided signature matches the payload and secret
func VerifyHMACSHA256(payload []byte, secret, expectedSig string) bool {
	actualSig := GenerateHMACSHA256(payload, secret)
	return hmac.Equal([]byte(actualSig), []byte(expectedSig))
}
