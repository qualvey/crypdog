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
	Jsonrpc string        `json:"jsonrpc"`
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
	BlockNumber      string   `json:"blockNumber"`
	TransactionHash  string   `json:"transactionHash"`
	TransactionIndex string   `json:"transactionIndex"`
	BlockHash        string   `json:"blockHash"`
	LogIndex         string   `json:"logIndex"`
	Removed          bool     `json:"removed"`
}

type EVMTransferEvent struct {
	TxHash          string
	FromAddress     string
	ToAddress       string
	ContractAddress string
	RawValue        *big.Int
	BlockNumber     int64
}

// GetLatestBlockNumber calls eth_blockNumber
func (c *EvmRPCClient) GetLatestBlockNumber(ctx context.Context) (int64, error) {
	reqBody := rpcRequest{
		Jsonrpc: "2.0",
		Method:  "eth_blockNumber",
		Params:  []interface{}{},
		ID:      1,
	}

	rawResult, err := c.doRPC(ctx, reqBody)
	if err != nil {
		return 0, err
	}

	var hexStr string
	if err := json.Unmarshal(rawResult, &hexStr); err != nil {
		return 0, fmt.Errorf("failed to unmarshal blockNumber: %w", err)
	}

	cleanHex := strings.TrimPrefix(hexStr, "0x")
	num, err := strconv.ParseInt(cleanHex, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse hex blockNumber %s: %w", hexStr, err)
	}

	return num, nil
}

// GetERC20Transfers calls eth_getLogs for ERC20 Transfer events to the target address
func (c *EvmRPCClient) GetERC20Transfers(ctx context.Context, targetAddress string, fromBlock, toBlock int64) ([]EVMTransferEvent, error) {
	// ERC20 Transfer topic
	transferSig := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

	// Format targetAddress as 32-byte topic (pad with zeroes on left)
	cleanAddr := strings.ToLower(strings.TrimPrefix(targetAddress, "0x"))
	if len(cleanAddr) != 40 {
		return nil, fmt.Errorf("invalid ethereum address length: %s", targetAddress)
	}
	topicTo := "0x000000000000000000000000" + cleanAddr

	filter := map[string]interface{}{
		"fromBlock": fmt.Sprintf("0x%x", fromBlock),
		"toBlock":   fmt.Sprintf("0x%x", toBlock),
		"topics": []interface{}{
			transferSig,
			nil, // any sender
			topicTo,
		},
	}

	reqBody := rpcRequest{
		Jsonrpc: "2.0",
		Method:  "eth_getLogs",
		Params:  []interface{}{filter},
		ID:      2,
	}

	rawResult, err := c.doRPC(ctx, reqBody)
	if err != nil {
		return nil, err
	}

	var logs []EVMLog
	if err := json.Unmarshal(rawResult, &logs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal eth_getLogs: %w", err)
	}

	var events []EVMTransferEvent
	for _, l := range logs {
		if l.Removed || len(l.Topics) < 3 {
			continue
		}

		from := "0x" + strings.TrimPrefix(l.Topics[1], "0x000000000000000000000000")
		to := "0x" + strings.TrimPrefix(l.Topics[2], "0x000000000000000000000000")

		dataClean := strings.TrimPrefix(l.Data, "0x")
		if dataClean == "" {
			continue
		}

		rawVal := new(big.Int)
		rawVal.SetString(dataClean, 16)

		blkClean := strings.TrimPrefix(l.BlockNumber, "0x")
		blkNum, _ := strconv.ParseInt(blkClean, 16, 64)

		events = append(events, EVMTransferEvent{
			TxHash:          l.TransactionHash,
			FromAddress:     strings.ToLower(from),
			ToAddress:       strings.ToLower(to),
			ContractAddress: strings.ToLower(l.Address),
			RawValue:        rawVal,
			BlockNumber:     blkNum,
		})
	}

	return events, nil
}

func (c *EvmRPCClient) doRPC(ctx context.Context, body rpcRequest) (json.RawMessage, error) {
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.rpcURL, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CrypDog/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(respBytes, &rpcResp); err != nil {
		return nil, fmt.Errorf("invalid json-rpc response: %s", string(respBytes))
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("rpc error (code %d): %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}
