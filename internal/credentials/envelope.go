// Package credentials seals app-scoped secrets independently of public run data.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

func Seal(key, aad, plaintext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("credential encryption is not configured")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(append([]byte{1}, nonce...), nonce, plaintext, aad), nil
}

func Open(key, aad, envelope []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("credential encryption is not configured")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(envelope) < 1+gcm.NonceSize()+gcm.Overhead() || envelope[0] != 1 {
		return nil, errors.New("invalid credential envelope")
	}
	plaintext, err := gcm.Open(nil, envelope[1:1+gcm.NonceSize()], envelope[1+gcm.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("cannot decrypt credential")
	}
	return plaintext, nil
}
