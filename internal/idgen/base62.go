/*
Package idgen 负责短码的生成与解析：用 Base62 把自增 ID 编码成短字符串（第 2、6 课）。

字母表是 0-9A-Za-z，共 62 个字符，大小写敏感。编码和解码共用同一个字母表；
一旦有数据产生，字母表的顺序就不能再改，否则旧短码会全部解析错。

EncodeBase62 只接受非负的 int64，负数属于调用方的程序错误，直接 panic。
DecodeBase62 把短码当作不可信输入，校验非法字符和 int64 溢出；
错误可以用 errors.Is 判断 ErrInvalidCode 或 ErrOverflow。
*/
package idgen

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// alphabet 的顺序就是短码的线格式，只能在末尾追加新字符，不能重排。
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base 是 alphabet 的长度，显式写成 int64：len 的结果是 int，不转换的话和 int64 混算会类型不匹配。
const base int64 = int64(len(alphabet))

var (
	// ErrInvalidCode 表示短码为空、包含字母表之外的字符，或不是 ASCII。
	ErrInvalidCode = errors.New("invalid base62 code")
	// ErrOverflow 表示短码解码后的值超出 int64 范围。
	ErrOverflow = errors.New("value overflows int64")
)

/*
decodeTable[c] 是字节 c 对应的数值，-1 表示非法。
int8 的零值是 0，而 0 也是字符 '0' 的合法值，所以必须先把整张表填成 -1。
*/
var decodeTable = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = int8(i)
	}
	return t
}()

/*
EncodeBase62 把非负整数 n 编码成 Base62 字符串，0 编码为 "0"。
n 为负数时 panic：短链 ID 来自非负的自增序列，负数说明调用方有 bug。
*/
func EncodeBase62(n int64) string {
	if n < 0 {
		panic("idgen: EncodeBase62 requires n >= 0")
	}
	if n == 0 {
		return "0"
	}

	// 取模先得到低位；int64 最多 11 位 Base62，容量按上限给，避免扩容。
	b := make([]byte, 0, 11)
	for n > 0 {
		b = append(b, alphabet[n%base])
		n /= base
	}
	// 上面是从低位到高位追加的，短码要求高位在前，所以返回前反转。
	slices.Reverse(b)
	return string(b)
}

/*
DecodeBase62 把 Base62 字符串解码成 int64。
空串和字母表之外的字符返回包装了 ErrInvalidCode 的错误，
数值超过 int64 返回包装了 ErrOverflow 的错误。
不要求规范形式："00" 和 "0" 都会解码成 0。
*/
func DecodeBase62(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("idgen: empty code: %w", ErrInvalidCode)
	}

	var n int64
	// 按字节遍历而不是 range：短码限定 ASCII，非 ASCII 输入的每个字节都查不到表。
	for i := 0; i < len(s); i++ {
		d := decodeTable[s[i]]
		if d < 0 {
			return 0, fmt.Errorf("idgen: invalid character %q at index %d: %w", s[i], i, ErrInvalidCode)
		}
		// 先判断再乘：n*base+d 溢出 int64 时会静默回绕，事后检查已经来不及。
		if n > (math.MaxInt64-int64(d))/base {
			return 0, fmt.Errorf("idgen: code %q: %w", s, ErrOverflow)
		}
		n = n*base + int64(d)
	}
	return n, nil
}
