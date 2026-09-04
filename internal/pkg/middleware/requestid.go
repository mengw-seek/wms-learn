package middleware

import (
	"crypto/rand"
	"encoding/hex"
)

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
