package webhook

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// SecretTokenPrefix 是对外展示和签发 Webhook 签名密钥的前缀。
	SecretTokenPrefix      = "whsec_"
	secretRandomBytes      = 32
	secretPrefixDisplayLen = 12

	// MasterKeyBytes 是 AES-256-GCM 主密钥长度（32 字节 = 64 个十六进制字符）。
	MasterKeyBytes      = 32
	ciphertextVersionV1 = byte(0x01)
	minCiphertextBytes  = 1 + 12 + 16 // 1B version + 12B nonce + 16B GCM tag
)

// ParseMasterKeyHex 解析并严格校验 64 字符十六进制主密钥（32 字节）。
func ParseMasterKeyHex(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) != MasterKeyBytes*2 {
		return nil, fmt.Errorf("WEBHOOK_SECRET_KEY must be %d hex characters", MasterKeyBytes*2)
	}
	key, err := hex.DecodeString(trimmed)
	if err != nil || len(key) != MasterKeyBytes {
		return nil, fmt.Errorf("WEBHOOK_SECRET_KEY must be valid hex")
	}
	return key, nil
}

// GenerateSecret 生成一条新的随机 Webhook 签名密钥及供界面识别的 12 字符前缀。
func GenerateSecret() (plaintext, prefix string, err error) {
	var raw [secretRandomBytes]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", "", fmt.Errorf("generate webhook secret: %w", err)
	}
	plaintext = SecretTokenPrefix + hex.EncodeToString(raw[:])
	prefix = plaintext[:secretPrefixDisplayLen]
	return plaintext, prefix, nil
}

/*
SecretCipher 使用 AES-256-GCM 加解密存储于数据库的 Webhook 签名密钥（ADR 0027）。
密文格式为 [1 字节版本 0x01][12 字节随机 Nonce][AES-GCM 密文与 Tag]，并绑定 tenant_id 作为 AAD 防止跨租户密文替换。
*/
type SecretCipher struct {
	aead cipher.AEAD
}

// NewSecretCipher 用 32 字节主密钥构造 SecretCipher。
func NewSecretCipher(masterKey []byte) (*SecretCipher, error) {
	if len(masterKey) != MasterKeyBytes {
		return nil, fmt.Errorf("master key must be %d bytes", MasterKeyBytes)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aes gcm: %w", err)
	}
	return &SecretCipher{aead: aead}, nil
}

// Encrypt 使用随机 Nonce 与租户绑定 AAD 加密明文密钥。
func (c *SecretCipher) Encrypt(tenantID int64, plaintext string) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("webhook cipher is not initialized")
	}
	if tenantID <= 0 {
		return nil, errors.New("tenant id must be positive")
	}
	if plaintext == "" {
		return nil, errors.New("secret plaintext must not be empty")
	}

	nonceSize := c.aead.NonceSize()
	out := make([]byte, 1+nonceSize, 1+nonceSize+len(plaintext)+c.aead.Overhead())
	out[0] = ciphertextVersionV1
	nonce := out[1 : 1+nonceSize]
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate webhook nonce: %w", err)
	}
	return c.aead.Seal(out, nonce, []byte(plaintext), tenantAAD(tenantID)), nil
}

// Decrypt 校验版本号、租户 AAD 与认证标签后还原明文签名密钥。
func (c *SecretCipher) Decrypt(tenantID int64, ciphertext []byte) (string, error) {
	if c == nil || c.aead == nil || tenantID <= 0 {
		return "", ErrDecryptSecret
	}
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < minCiphertextBytes || len(ciphertext) < 1+nonceSize+c.aead.Overhead() {
		return "", ErrDecryptSecret
	}
	if ciphertext[0] != ciphertextVersionV1 {
		return "", fmt.Errorf("%w: unsupported version %d", ErrDecryptSecret, ciphertext[0])
	}
	nonce := ciphertext[1 : 1+nonceSize]
	sealed := ciphertext[1+nonceSize:]
	plain, err := c.aead.Open(nil, nonce, sealed, tenantAAD(tenantID))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrDecryptSecret, err)
	}
	return string(plain), nil
}

func tenantAAD(tenantID int64) []byte {
	return []byte("shortlink:webhook-secret:v1:tenant:" + strconv.FormatInt(tenantID, 10))
}
