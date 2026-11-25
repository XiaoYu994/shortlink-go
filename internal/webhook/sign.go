package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderEvent     = "X-Shortlink-Event"
	HeaderDelivery  = "X-Shortlink-Delivery"
	HeaderSignature = "X-Shortlink-Signature"
	UserAgent       = "shortlink-webhook/1.0"
)

// FormatDeliveryID 返回随任务全重试周期保持不变的投递幂等编号（dlv_<job_id>）。
func FormatDeliveryID(jobID int64) string {
	return "dlv_" + strconv.FormatInt(jobID, 10)
}

/*
Sign 按 t=<unix_sec>,v1=<hex(HMAC-SHA256(secret, "<unix_sec>.<payload>"))> 生成签名请求头（ADR 0027）。
把时间戳与原始请求体字节拼在一起签名，可防止抓包重放与空白篡改。
*/
func Sign(secret string, ts time.Time, payload []byte) string {
	unixSec := strconv.FormatInt(ts.Unix(), 10)
	sig := computeHMAC(secret, unixSec, payload)
	return "t=" + unixSec + ",v1=" + hex.EncodeToString(sig)
}

/*
VerifySignature 校验 X-Shortlink-Signature 请求头与当前时间重放窗口（tolerance <= 0 时跳过时间窗检查）。
使用 hmac.Equal 执行常量时间比对，避免时序侧信道泄露。
*/
func VerifySignature(secret, header string, payload []byte, now time.Time, tolerance time.Duration) error {
	if secret == "" {
		return ErrInvalidSignature
	}
	var (
		tsStr string
		sigs  [][]byte
	)
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "t":
			if tsStr != "" {
				return ErrInvalidSignature
			}
			tsStr = v
		case "v1":
			decoded, err := hex.DecodeString(v)
			if err != nil || len(decoded) != sha256.Size {
				return ErrInvalidSignature
			}
			sigs = append(sigs, decoded)
		}
	}
	if tsStr == "" || len(sigs) == 0 {
		return ErrInvalidSignature
	}
	tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return ErrInvalidSignature
	}
	if tolerance > 0 {
		diff := now.Sub(time.Unix(tsUnix, 0))
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			return fmt.Errorf("%w: age %s exceeds %s", ErrSignatureExpired, diff, tolerance)
		}
	}
	expected := computeHMAC(secret, tsStr, payload)
	for _, sig := range sigs {
		if hmac.Equal(sig, expected) {
			return nil
		}
	}
	return ErrInvalidSignature
}

func computeHMAC(secret, tsStr string, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(tsStr))
	_, _ = mac.Write([]byte{'.'})
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}
