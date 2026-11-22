package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const tokenBytes = 32

/*
TokenFormat 描述带前缀的小写十六进制不透明凭证格式。
调用方只使用包里导出的 APIKeyToken 与 SessionToken；零值 TokenFormat{} 不代表有效凭证。
*/
type TokenFormat struct {
	prefix string
}

var (
	// APIKeyToken 描述 API Key 的明文格式（slk_ 加 64 位小写十六进制）。
	APIKeyToken = TokenFormat{prefix: "slk_"}
	// SessionToken 描述服务端会话令牌的明文格式（sls_ 加 64 位小写十六进制）。
	SessionToken = TokenFormat{prefix: "sls_"}
)

// Valid 检查 token 是否符合当前前缀加 64 位小写十六进制格式。
func (f TokenFormat) Valid(token string) bool {
	if f.prefix == "" || len(token) != len(f.prefix)+tokenBytes*2 {
		return false
	}
	if token[:len(f.prefix)] != f.prefix {
		return false
	}
	for _, c := range token[len(f.prefix):] {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

/*
Generate 用 crypto/rand 生成一条新凭证。
plaintext 只交给创建方或写入 HttpOnly Cookie；hash 写入数据库（api_keys.key_hash 或 sessions.token_hash）。
*/
func (f TokenFormat) Generate() (plaintext string, hash []byte, err error) {
	if f.prefix == "" {
		return "", nil, errors.New("token format prefix is empty")
	}
	var buf [tokenBytes]byte
	if _, err = rand.Read(buf[:]); err != nil {
		return "", nil, err
	}
	plaintext = f.prefix + hex.EncodeToString(buf[:])
	return plaintext, HashToken(plaintext), nil
}

/*
HashToken 计算写入数据库的 32 字节 SHA-256 哈希。
调用方在入库或查库前应先按对应的 TokenFormat.Valid 校验格式。
*/
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
