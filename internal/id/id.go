package id

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

func New(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return hex.EncodeToString(b[:])
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
