package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type EvmRPCClient struct {
	rpcURL string
	client *http.Client
}

func NewEvmRPCClient(rpcURL string) *EvmRPCClient {
	return &EvmRPCClient{
		rpcURL: rpcURL,
		client: &http.Client{
			Timeout: 12 * time.Second,
		},
	}
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

type rpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type EVMLog struct {
	Address          string   `json:"address"`
	Topics           []string `json:"topics"`
	Data             string   `json:"data"`
	TransactionHash  string   `json:"transactionHash"`
	TransactionIndex string   `json:"transactionIndex"`
	// RPC 返回的原始 0x 十六进制字符串
	BlockNumberHex string `json:"blockNumber"`
	LogIndexHex    string `json:"logIndex"`
	BlockNumber    uint64 `json:"-"`
	LogIndex       int64  `json:"-"`
	Removed        bool   `json:"removed"`
}

type EVMTransferEvent struct {
	TxHash          string
	FromAddress     string
	ToAddress       string
	ContractAddress string
	RawValue        *big.Int
	BlockNumber     int64
}

// 1. evm_rpc_client.go 中：统一提供高效的 GetLatestBlockNumber
func (c *EvmRPCClient) GetLatestBlockNumber(ctx context.Context) (uint64, error) {
	raw, err := c.doRPC(ctx, rpcRequest{
		JSONRPC: "2.0",
		Method:  "eth_blockNumber",
		Params:  []interface{}{},
		ID:      1,
	})
	if err != nil {
		return 0, err
	}

	var hexBlock string
	if err := json.Unmarshal(raw, &hexBlock); err != nil {
		return 0, fmt.Errorf("unmarshal block hex failed: %w", err)
	}

	cleanHex := strings.TrimPrefix(strings.ToLower(hexBlock), "0x")
	num, err := strconv.ParseUint(cleanHex, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse hex (%s) to uint64 failed: %w", hexBlock, err)
	}
	return num, nil
}

// GetERC20Logs 通用的 ERC20 Transfer 事件拉取
func (c *EvmRPCClient) GetERC20Logs(ctx context.Context, fromBlock, toBlock uint64, targetAddresses ...string) ([]EVMLog, error) {
	transferSig := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

	filter := map[string]interface{}{
		"fromBlock": fmt.Sprintf("0x%x", fromBlock),
		"toBlock":   fmt.Sprintf("0x%x", toBlock),
	}

	if len(targetAddresses) == 0 {
		// 监听该区块内所有的 Transfer 事件
		filter["topics"] = []interface{}{transferSig}
	} else {
		// 精准过滤：利用以太坊 RPC 支持的数组模式匹配多个收款地址
		topicsTo := make([]string, 0, len(targetAddresses))
		for _, addr := range targetAddresses {
			clean := strings.ToLower(strings.TrimPrefix(addr, "0x"))
			if len(clean) == 40 {
				topicsTo = append(topicsTo, "0x000000000000000000000000"+clean)
			}
		}
		filter["topics"] = []interface{}{transferSig, nil, topicsTo}
	}

	raw, err := c.doRPC(ctx, rpcRequest{
		JSONRPC: "2.0",
		Method:  "eth_getLogs",
		Params:  []interface{}{filter},
		ID:      2,
	})
	if err != nil {
		return nil, err
	}

	var logs []EVMLog
	if err := json.Unmarshal(raw, &logs); err != nil {
		return nil, fmt.Errorf("unmarshal eth_getLogs failed: %w", err)
	}

	// 统一在底层完成十六进制 blockNumber 解析
	for i := range logs {
		// 1. 解析 BlockNumber (uint64)
		cleanBlock := strings.TrimPrefix(strings.ToLower(logs[i].BlockNumberHex), "0x")
		logs[i].BlockNumber, _ = strconv.ParseUint(cleanBlock, 16, 64)
		// 2. 解析 LogIndex (int64)
		cleanLogIdx := strings.TrimPrefix(strings.ToLower(logs[i].LogIndexHex), "0x")
		logs[i].LogIndex, _ = strconv.ParseInt(cleanLogIdx, 16, 64)
	}

	return logs, nil
}

func (c *EvmRPCClient) doRPC(ctx context.Context, body rpcRequest) (json.RawMessage, error) {
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal rpc request failed: %w", err)
	}

	// 1. 使用 bytes.NewReader，避免内存额外拷贝
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpcURL, bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("create http request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CrypDog/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute rpc failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body failed: %w", err)
	}

	// 2. 融入 callRPC 的优点：先校验 HTTP 状态码（拦截反代 401/403/502 等非 200 情况）
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rpc node returned http %d: %s", resp.StatusCode, string(respBytes))
	}

	// 3. 保持 doRPC 的优点：解析并校验 JSON-RPC 2.0 规范的业务 error
	var rpcResp rpcResponse
	if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
		return nil, fmt.Errorf("invalid json-rpc response: %s", string(respBytes))
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error (code %d): %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	// 4. 直接解包返回干净的 Result
	return rpcResp.Result, nil
}
