package model

import (
	"strings"
)

// NormalizeAddress 对各公链钱包地址进行规范化处理：
// 1. EVM / Hex 体系（以 0x 开头）：去除两端空格并强制转全小写
// 2. Base58 体系（TRON、Solana、BTC 传统地址）：仅去除两端空格，严格保留大小写
// 3. Bech32 体系（BTC SegWit/Taproot bc1 开头、Cosmos 开头）：去除两端空格并强制转全小写
func (c Chain) NormalizeAddress(rawAddr string) string {
	addr := strings.TrimSpace(rawAddr)
	if addr == "" {
		return ""
	}

	// 规则 1：明确为 Base58 体系的链，严格区分大小写，只保留 TrimSpace
	switch strings.ToUpper(string(c)) {
	case "TRON", "SOLANA", "BTC_LEGACY":
		return addr
	}

	// 规则 2：Hex 地址（0x 开头，如 EVM、Layer2、Polygon、Arbitrum 等）强制转小写
	if len(addr) >= 2 && (addr[:2] == "0x" || addr[:2] == "0X") {
		return strings.ToLower(addr)
	}
	// 规则 3：Bech32 编码（如比特币原生隔离见证 bc1 开头、Cosmos 系列），规范要求全小写
	if strings.HasPrefix(strings.ToLower(addr), "bc1") ||
		strings.HasPrefix(strings.ToLower(addr), "cosmos") {
		return strings.ToLower(addr)
	}

	// 默认兜底：如果不属于上述特殊链且不是 0x，保留原字符（避免意外破坏未知链地址）
	return addr
}

// NormalizeChain 将各种链别名（如 TRC20, ERC20, BEP20）规范化为系统内部标准 Chain
func NormalizeChain(rawChain string) Chain {
	c := strings.ToUpper(strings.TrimSpace(rawChain))
	switch c {
	case "TRC20", "TRX", "TRON":
		return ChainTron
	case "ERC20", "ETH", "ETHEREUM":
		return ChainEth
	case "BEP20", "BSC", "BINANCE":
		return ChainBsc
	case "SOL", "SOLANA":
		return ChainSolana
	case "POLYGON", "MATIC":
		return ChainPolygon
	case "ARBITRUM", "ARB":
		return ChainArbitrum
	default:
		return Chain(c)
	}
}

