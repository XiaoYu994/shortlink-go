/*
Package idgen 负责随机短码的生成（第 2、6、7 课）。

字母表是 0-9A-Za-z，共 62 个字符，大小写敏感。
GenerateCode 用 crypto/rand 生成随机短码，短码对外公开，不能被预测。
*/
package idgen

import (
	"crypto/rand"
	"fmt"
)

// alphabet 是短码使用的字符集：0-9、A-Z、a-z，共 62 个。
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base 是 alphabet 的长度，用来把随机字节映射到字母表。
const base = len(alphabet)

// CodeLength 是随机短码的长度；8 位 Base62 约有 2.18e14 种组合。
const CodeLength = 8

// maxUnbiasedByte 是拒绝采样的上界：256 % 62 = 8，248 及以上的字节会造成取模偏差。
const maxUnbiasedByte = 248

/*
GenerateCode 生成 CodeLength 位随机短码，字符来自 Base62 字母表。
它用 crypto/rand 而不是 math/rand：短码对外公开，不能被预测。
*/
func GenerateCode() (string, error) {
	code := make([]byte, CodeLength)
	buf := make([]byte, CodeLength)
	pos := len(buf)
	for i := range code {
		for {
			if pos == len(buf) {
				if _, err := rand.Read(buf); err != nil {
					return "", fmt.Errorf("idgen: generate code: %w", err)
				}
				pos = 0
			}
			b := buf[pos]
			pos++
			if b < maxUnbiasedByte {
				code[i] = alphabet[b%byte(base)]
				break
			}
		}
	}
	return string(code), nil
}
