/*
Package tenant 表示短链的租户和 API Key（阶段 6）。

明文密钥只在生成时交给调用方。库存 SHA-256。
格式不对的密钥不会去查数据库。
*/
package tenant

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// ErrUnauthenticated 表示没有可用的 API Key。HTTP 层映射成 401。
var ErrUnauthenticated = errors.New("unauthenticated")

/*
Tenant 是鉴权通过后放进 context 的身份。
ID 是本地主键。ExternalID 留给以后的平台租户，没有时为空。
*/
type Tenant struct {
	ID         int64
	ExternalID string
}

const (
	apiKeyPrefix = "slk_"
	apiKeyBytes  = 32
)

// ValidToken 检查密钥是不是 slk_ 加 64 位小写十六进制。
func ValidToken(token string) bool {
	if len(token) != len(apiKeyPrefix)+apiKeyBytes*2 {
		return false
	}
	if token[:len(apiKeyPrefix)] != apiKeyPrefix {
		return false
	}
	for _, c := range token[len(apiKeyPrefix):] {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// HashAPIKey 计算库存的哈希。调用方先过 ValidToken。
func HashAPIKey(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

/*
GenerateAPIKey 生成一把新密钥。
plaintext 只给创建者看一次；hash 写入 api_keys.key_hash。
*/
func GenerateAPIKey() (plaintext string, hash []byte, err error) {
	var buf [apiKeyBytes]byte
	if _, err = rand.Read(buf[:]); err != nil {
		return "", nil, err
	}
	plaintext = apiKeyPrefix + hex.EncodeToString(buf[:])
	return plaintext, HashAPIKey(plaintext), nil
}
