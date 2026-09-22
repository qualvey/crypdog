package signature

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
)

var (
	ErrInvalidWebhookURL  = errors.New("invalid webhook url")
	ErrDisallowedScheme   = errors.New("webhook url scheme must be http or https")
	ErrSSRFProtection     = errors.New("webhook url targets restricted or private infrastructure")
	ErrUnspecifiedAddress = errors.New("webhook url cannot use an unspecified address")
	ErrInvalidAddress     = errors.New("invalid wallet address format")
)

// ValidateWebhookURL 校验 Webhook URL 的合法性与 SSRF 安全防御
// allowLocal 用于本地开发或单测时允许 localhost
func ValidateWebhookURL(rawURL string, allowLocal bool) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ErrInvalidWebhookURL
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidWebhookURL, err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ErrDisallowedScheme
	}

	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("%w: missing host", ErrInvalidWebhookURL)
	}

	// 1. 拦截云厂商元数据地址 (AWS/GCP/Azure/Alibaba Cloud)
	if hostname == "169.254.169.254" || strings.HasPrefix(hostname, "169.254.") {
		return fmt.Errorf("%w: cloud metadata address forbidden", ErrSSRFProtection)
	}

	// 2. 检查 IP 地址
	ip := net.ParseIP(hostname)
	if ip != nil {
		if ip.IsUnspecified() {
			return fmt.Errorf("%w: %w; use localhost/127.0.0.1 for local development or a reachable host name", ErrSSRFProtection, ErrUnspecifiedAddress)
		}
		if ip.IsMulticast() {
			return fmt.Errorf("%w: multicast address forbidden", ErrSSRFProtection)
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("%w: link-local address forbidden", ErrSSRFProtection)
		}
		if ip.IsLoopback() && !allowLocal {
			return fmt.Errorf("%w: loopback address forbidden in production", ErrSSRFProtection)
		}
		if ip.IsPrivate() && !allowLocal {
			return fmt.Errorf("%w: private network address forbidden in production", ErrSSRFProtection)
		}
		return nil
	}

	// 3. 检查常见危险域名
	lowerHost := strings.ToLower(hostname)
	if !allowLocal && (lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".local") || strings.HasSuffix(lowerHost, ".internal") || strings.HasSuffix(lowerHost, ".lan")) {
		return fmt.Errorf("%w: local domain forbidden in production", ErrSSRFProtection)
	}

	return nil
}

var b58Alphabet = []byte("123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz")
var b58Index = func() [256]int8 {
	var idx [256]int8
	for i := range idx {
		idx[i] = -1
	}
	for i, c := range b58Alphabet {
		idx[c] = int8(i)
	}
	return idx
}()

func decodeBase58(s string) ([]byte, error) {
	if len(s) == 0 {
		return nil, errors.New("empty string")
	}
	zeroCount := 0
	for zeroCount < len(s) && s[zeroCount] == '1' {
		zeroCount++
	}

	n := new(big.Int)
	b58Radix := big.NewInt(58)
	for i := zeroCount; i < len(s); i++ {
		val := b58Index[s[i]]
		if val == -1 {
			return nil, errors.New("invalid base58 character")
		}
		n.Mul(n, b58Radix)
		n.Add(n, big.NewInt(int64(val)))
	}

	b := n.Bytes()
	result := make([]byte, zeroCount+len(b))
	copy(result[zeroCount:], b)
	return result, nil
}

// ValidateChainAddress 严格核验各公链钱包地址格式及校验和
func ValidateChainAddress(chain string, rawAddr string) error {
	addr := strings.TrimSpace(rawAddr)
	if addr == "" {
		return fmt.Errorf("%w: address cannot be empty", ErrInvalidAddress)
	}

	normChain := strings.ToUpper(strings.TrimSpace(chain))
	switch normChain {
	case "TRON", "TRC20", "TRX":
		// TRON: Base58Check 格式，以 'T' 开头，长度必须为 34
		if len(addr) != 34 || addr[0] != 'T' {
			return fmt.Errorf("%w: TRON address must start with 'T' and be 34 characters long", ErrInvalidAddress)
		}
		decoded, err := decodeBase58(addr)
		if err != nil {
			return fmt.Errorf("%w: invalid base58 encoding: %v", ErrInvalidAddress, err)
		}
		if len(decoded) != 25 {
			return fmt.Errorf("%w: invalid TRON address byte length (%d)", ErrInvalidAddress, len(decoded))
		}
		if decoded[0] != 0x41 {
			return fmt.Errorf("%w: invalid TRON address prefix (expected 0x41)", ErrInvalidAddress)
		}
		// Base58Check: 校验前 21 字节的 double SHA256 校验和
		h1 := sha256.Sum256(decoded[:21])
		h2 := sha256.Sum256(h1[:])
		if !bytes.Equal(decoded[21:25], h2[:4]) {
			return fmt.Errorf("%w: TRON address checksum mismatch", ErrInvalidAddress)
		}
		return nil

	case "SOLANA", "SOL":
		// Solana: Base58 编码的 32 字节 Ed25519 公钥，通常长度在 32 ~ 44 之间
		if len(addr) < 32 || len(addr) > 44 {
			return fmt.Errorf("%w: Solana address length must be between 32 and 44 characters", ErrInvalidAddress)
		}
		decoded, err := decodeBase58(addr)
		if err != nil {
			return fmt.Errorf("%w: invalid Solana base58 address: %v", ErrInvalidAddress, err)
		}
		if len(decoded) != 32 {
			return fmt.Errorf("%w: Solana public key must be 32 bytes (got %d)", ErrInvalidAddress, len(decoded))
		}
		return nil

	case "ETH", "ETHEREUM", "ERC20", "BSC", "BEP20", "POLYGON", "MATIC", "ARBITRUM", "ARB", "OPTIMISM", "BASE", "AVAX":
		// EVM 体系: 0x 开头，后跟 40 个十六进制字符
		if len(addr) != 42 || !(strings.HasPrefix(addr, "0x") || strings.HasPrefix(addr, "0X")) {
			return fmt.Errorf("%w: EVM address must start with 0x and be 42 characters long", ErrInvalidAddress)
		}
		for _, c := range addr[2:] {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return fmt.Errorf("%w: EVM address contains non-hexadecimal character", ErrInvalidAddress)
			}
		}
		return nil

	default:
		// 未知公链时做基础防空与长度保护
		if len(addr) < 10 || len(addr) > 128 {
			return fmt.Errorf("%w: invalid address length for chain %s", ErrInvalidAddress, chain)
		}
		return nil
	}
}
