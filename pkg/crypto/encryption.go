package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ApplicationEncryptor управляет шифрованием данных на уровне приложения.
// Использует ключ из переменной окружения APP_ENCRYPTION_KEY (или APP_ENCRYPTION_KEY_FILE).
// Этот ключ монтируется как Docker secret во все микросервисы.
type ApplicationEncryptor struct {
	key    [32]byte
	keySet bool
	mu     sync.RWMutex
}

var (
	globalEncryptor     *ApplicationEncryptor
	globalEncryptorOnce sync.Once
)

// GetGlobalEncryptor возвращает singleton ApplicationEncryptor.
// Автоматически инициализируется при первом вызове из переменной окружения.
func GetGlobalEncryptor() (*ApplicationEncryptor, error) {
	var initErr error
	globalEncryptorOnce.Do(func() {
		globalEncryptor = &ApplicationEncryptor{}
		initErr = globalEncryptor.LoadKey()
	})
	if initErr != nil {
		return nil, initErr
	}
	if !globalEncryptor.IsKeySet() {
		return nil, fmt.Errorf("application encryption key не установлен")
	}
	return globalEncryptor, nil
}

// LoadKey загружает ключ шифрования из переменных окружения.
// Порядок поиска:
//  1. APP_ENCRYPTION_KEY_FILE — путь к файлу с ключом (Docker secret)
//  2. APP_ENCRYPTION_KEY — ключ напрямую в env (для dev окружения)
func (e *ApplicationEncryptor) LoadKey() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 1. Пробуем загрузить из файла (production with Docker secrets)
	if keyFile := strings.TrimSpace(os.Getenv("APP_ENCRYPTION_KEY_FILE")); keyFile != "" {
		keyBytes, err := os.ReadFile(keyFile)
		if err != nil {
			return fmt.Errorf("ошибка чтения APP_ENCRYPTION_KEY_FILE: %w", err)
		}
		keyStr := strings.TrimSpace(string(keyBytes))
		if keyStr == "" {
			return fmt.Errorf("APP_ENCRYPTION_KEY_FILE пустой")
		}
		e.key = sha256.Sum256([]byte(keyStr))
		e.keySet = true
		return nil
	}

	// 2. Пробуем загрузить из переменной окружения (development)
	if keyStr := strings.TrimSpace(os.Getenv("APP_ENCRYPTION_KEY")); keyStr != "" {
		e.key = sha256.Sum256([]byte(keyStr))
		e.keySet = true
		return nil
	}

	// Ключ не найден — работаем без шифрования (backward compatibility)
	e.keySet = false
	return nil
}

// IsKeySet проверяет, установлен ли ключ шифрования.
func (e *ApplicationEncryptor) IsKeySet() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.keySet
}

// EncryptField шифрует данные с использованием application-level ключа.
// Префикс: "$app$" (отличается от "$mk$" для user MasterKey).
func (e *ApplicationEncryptor) EncryptField(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	e.mu.RLock()
	if !e.keySet {
		e.mu.RUnlock()
		return plaintext, nil // Нет ключа — возвращаем plaintext (backward compat)
	}
	key := e.key
	e.mu.RUnlock()

	block, err := aes.NewCipher(key[:])
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
	return "$app$" + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptField расшифровывает данные, зашифрованные EncryptField.
// Поддерживает префиксы: "$app$" (application key) и plaintext (backward compat).
func (e *ApplicationEncryptor) DecryptField(encrypted string) (string, error) {
	if encrypted == "" || !strings.HasPrefix(encrypted, "$app$") {
		return encrypted, nil // Plaintext или пустая строка
	}

	e.mu.RLock()
	if !e.keySet {
		e.mu.RUnlock()
		return "", fmt.Errorf("данные зашифрованы, но APP_ENCRYPTION_KEY не установлен")
	}
	key := e.key
	e.mu.RUnlock()

	rawB64 := strings.TrimPrefix(encrypted, "$app$")
	ciphertext, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}

	block, err := aes.NewCipher(key[:])
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

// IsEncrypted проверяет, зашифровано ли значение application ключом.
func IsEncryptedWithAppKey(value string) bool {
	return strings.HasPrefix(value, "$app$")
}
