package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomID returns n characters of [a-z0-9], the shape E2B uses for sandbox
// and template ids (the orchestrator proxy rejects anything else).
func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}

	return string(b)
}

func newSandboxID() string  { return randomID(20) }
func newTemplateID() string { return randomID(20) }

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}

	return hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
