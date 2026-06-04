package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

// EncryptedFieldPrefix — префикс для полей, зашифрованных MasterKey
const EncryptedFieldPrefix = "$mk$"

// EncryptFieldWithMasterKey шифрует plaintext с использованием AES-256-GCM.
// masterKey должен быть [32]byte (256 бит).
// Возвращает строку: "$mk$" + base64(nonce + ciphertext).
func EncryptFieldWithMasterKey(masterKey [32]byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	block, err := aes.NewCipher(masterKey[:])
	if err != nil {
		return "", fmt.Errorf("aes.NewCipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cipher.NewGCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("nonce generation: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return EncryptedFieldPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptFieldWithMasterKey расшифровывает значение, зашифрованное EncryptFieldWithMasterKey.
// Если значение не начинается с "$mk$" — возвращает как есть (plaintext compatibility).
func DecryptFieldWithMasterKey(masterKey [32]byte, encrypted string) (string, error) {
	if encrypted == "" || !IsEncryptedWithMasterKey(encrypted) {
		return encrypted, nil
	}

	rawB64 := strings.TrimPrefix(encrypted, EncryptedFieldPrefix)
	ciphertext, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}

	block, err := aes.NewCipher(masterKey[:])
	if err != nil {
		return "", fmt.Errorf("aes.NewCipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cipher.NewGCM: %w", err)
	}

	if len(ciphertext) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, payload := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, payload, nil)
	if err != nil {
		return "", fmt.Errorf("gcm.Open: %w", err)
	}

	return string(plaintext), nil
}

// IsEncryptedWithMasterKey проверяет, зашифровано ли значение MasterKey (префикс "$mk$").
func IsEncryptedWithMasterKey(value string) bool {
	return strings.HasPrefix(value, EncryptedFieldPrefix)
}
