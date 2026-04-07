package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// SolanaIndexer 用 HTTP polling 監聽 Solana 上的 CCTP 事件
// Solana 的 CCTP 事件是 Anchor 事件，存在 transaction logs 裡
type SolanaIndexer struct {
	rpcURL      string
	programID   string
	httpClient  *http.Client
	logger      *zap.Logger
	lastSig     string // 上次看到的最新 signature，用來做 pagination
}

// NewSolana 建立一個 Solana CCTP indexer
// programID 是 MessageTransmitter 的 program address
func NewSolana(rpcURL, programID string, logger *zap.Logger) *SolanaIndexer {
	return &SolanaIndexer{
		rpcURL:     rpcURL,
		programID:  programID,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     logger,
	}
}

// anchorDiscriminator 計算 Anchor event discriminator
// discriminator = sha256("event:<EventName>")[0:8]
func anchorDiscriminator(eventName string) [8]byte {
	h := sha256.Sum256([]byte("event:" + eventName))
	var d [8]byte
	copy(d[:], h[:8])
	return d
}

var (
	messageSentDiscriminator     = anchorDiscriminator("MessageSent")
	messageReceivedDiscriminator = anchorDiscriminator("MessageReceived")
)

// Start 開始用 polling 方式監聽 Solana 事件
// lastSig 是上次處理到的 signature（從 checkpoint 載入），空字串代表從最新開始
func (s *SolanaIndexer) Start(ctx context.Context, lastSig string, eventCh chan<- Event, saveFn func(string) error) {
	s.lastSig = lastSig
	s.logger.Info("開始監聽 Solana CCTP 事件",
		zap.String("program", s.programID),
		zap.String("lastSig", lastSig),
	)

	// 先做一次歷史補掃：把啟動前漏掉的 tx 一次抓完
	s.catchUp(ctx, eventCh)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("停止監聽 Solana")
			return
		case <-ticker.C:
			prevSig := s.lastSig
			s.poll(ctx, eventCh)
			if s.lastSig != prevSig && saveFn != nil {
				if err := saveFn(s.lastSig); err != nil {
					s.logger.Error("儲存 Solana checkpoint 失敗", zap.Error(err))
				}
			}
		}
	}
}

// catchUp 啟動時一次性拉取 lastSig 之後所有遺漏的 tx（最多 1000 筆）
// 若有 lastSig（從 DB 載入），則只抓該 sig 之後的新 tx
func (s *SolanaIndexer) catchUp(ctx context.Context, eventCh chan<- Event) {

	s.logger.Info("Solana 歷史補掃開始（最多 1000 筆）", zap.String("since", s.lastSig))

	// 以 before 往前翻頁，直到沒有更多資料或碰到 lastSig
	var before string
	resumeSig := s.lastSig // 只補這之後的 tx
	total := 0

	for total < 1000 {
		params := []any{
			s.programID,
			map[string]any{
				"limit":      100,
				"commitment": "confirmed",
			},
		}
		if before != "" {
			params[1].(map[string]any)["before"] = before
		}
		if resumeSig != "" {
			params[1].(map[string]any)["until"] = resumeSig
		}

		resp, err := s.rpcCall(ctx, "getSignaturesForAddress", params)
		if err != nil || ctx.Err() != nil {
			break
		}

		var sigs []solanaSignatureResult
		if err := json.Unmarshal(resp, &sigs); err != nil || len(sigs) == 0 {
			break
		}

		// 第一頁的第一筆就是最新的，記下來做為未來 poll 的 until
		if total == 0 && s.lastSig == "" {
			s.lastSig = sigs[0].Signature
		}

		for _, sig := range sigs {
			if sig.Err != nil {
				continue
			}
			events, err := s.parseTransaction(ctx, sig.Signature, sig.BlockTime)
			if err != nil {
				continue
			}
			for _, e := range events {
				eventCh <- e
			}
		}

		total += len(sigs)
		before = sigs[len(sigs)-1].Signature

		// 不足 100 筆代表已到底
		if len(sigs) < 100 {
			break
		}
	}

	s.logger.Info("Solana 歷史補掃完成", zap.Int("processed", total))
}

// poll 查詢最新的 transactions 並解析事件
func (s *SolanaIndexer) poll(ctx context.Context, eventCh chan<- Event) {
	sigs, err := s.getSignatures(ctx)
	if err != nil {
		s.logger.Error("Solana getSignaturesForAddress 失敗", zap.Error(err))
		return
	}

	if len(sigs) == 0 {
		return
	}

	// 更新 lastSig，下次 poll 從這裡開始（避免重複處理）
	newLastSig := sigs[0].Signature

	for _, sig := range sigs {
		events, err := s.parseTransaction(ctx, sig.Signature, sig.BlockTime)
		if err != nil {
			s.logger.Debug("解析 Solana transaction 失敗",
				zap.String("sig", sig.Signature),
				zap.Error(err),
			)
			continue
		}
		for _, e := range events {
			eventCh <- e
		}
	}

	s.lastSig = newLastSig
}

// solanaSignatureResult 是 getSignaturesForAddress 回傳的單筆資料
type solanaSignatureResult struct {
	Signature string `json:"signature"`
	BlockTime int64  `json:"blockTime"`
	Err       any    `json:"err"`
}

// getSignatures 取得最新的 transaction signatures
func (s *SolanaIndexer) getSignatures(ctx context.Context) ([]solanaSignatureResult, error) {
	params := []any{
		s.programID,
		map[string]any{
			"limit":      20,
			"commitment": "confirmed",
		},
	}
	if s.lastSig != "" {
		params[1].(map[string]any)["until"] = s.lastSig
	}

	resp, err := s.rpcCall(ctx, "getSignaturesForAddress", params)
	if err != nil {
		return nil, err
	}

	var sigs []solanaSignatureResult
	if err := json.Unmarshal(resp, &sigs); err != nil {
		return nil, fmt.Errorf("解析 signatures 失敗: %w", err)
	}

	// 只回傳成功的交易
	var ok []solanaSignatureResult
	for _, sig := range sigs {
		if sig.Err == nil {
			ok = append(ok, sig)
		}
	}
	return ok, nil
}

// solanaTransaction 是我們需要的 transaction 欄位
type solanaTransaction struct {
	Meta struct {
		LogMessages []string `json:"logMessages"`
	} `json:"meta"`
	BlockTime int64 `json:"blockTime"`
}

// parseTransaction 取得單筆 transaction 並解析出 CCTP 事件
func (s *SolanaIndexer) parseTransaction(ctx context.Context, signature string, blockTimestamp int64) ([]Event, error) {
	params := []any{
		signature,
		map[string]any{
			"encoding":   "json",
			"commitment": "confirmed",
		},
	}

	resp, err := s.rpcCall(ctx, "getTransaction", params)
	if err != nil {
		return nil, err
	}

	var tx solanaTransaction
	if err := json.Unmarshal(resp, &tx); err != nil {
		return nil, fmt.Errorf("解析 transaction 失敗: %w", err)
	}

	blockTime := time.Unix(blockTimestamp, 0)
	var events []Event

	for _, logMsg := range tx.Meta.LogMessages {
		// Anchor 事件的 log 格式：「Program data: <base64>」
		const prefix = "Program data: "
		if !strings.HasPrefix(logMsg, prefix) {
			continue
		}

		data, err := base64.StdEncoding.DecodeString(logMsg[len(prefix):])
		if err != nil || len(data) < 8 {
			continue
		}

		var disc [8]byte
		copy(disc[:], data[:8])
		payload := data[8:]

		switch disc {
		case messageSentDiscriminator:
			e, err := parseSolanaMessageSent(payload, signature, blockTime)
			if err != nil {
				s.logger.Debug("解析 Solana MessageSent 失敗", zap.Error(err))
				continue
			}
			events = append(events, e)

		case messageReceivedDiscriminator:
			e, err := parseSolanaMessageReceived(payload, signature, blockTime)
			if err != nil {
				s.logger.Debug("解析 Solana MessageReceived 失敗", zap.Error(err))
				continue
			}
			events = append(events, e)
		}
	}

	return events, nil
}

// parseSolanaMessageSent 解析 Borsh 編碼的 MessageSent 事件
// struct MessageSent { message: Vec<u8> }
func parseSolanaMessageSent(data []byte, txHash string, blockTime time.Time) (Event, error) {
	if len(data) < 4 {
		return Event{}, fmt.Errorf("data 太短")
	}

	// Borsh Vec<u8>: 4 bytes LE length prefix + bytes
	msgLen := binary.LittleEndian.Uint32(data[0:4])
	if len(data) < int(4+msgLen) {
		return Event{}, fmt.Errorf("message 長度不符")
	}
	message := data[4 : 4+msgLen]

	if len(message) < 84 {
		return Event{}, fmt.Errorf("message 太短: %d bytes", len(message))
	}

	destDomain := binary.BigEndian.Uint32(message[8:12])
	nonce := binary.BigEndian.Uint64(message[12:20])
	sender := fmt.Sprintf("0x%x", message[20:52])
	recipient := fmt.Sprintf("0x%x", message[52:84])

	destChain, ok := DomainToChain[destDomain]
	if !ok {
		destChain = fmt.Sprintf("domain_%d", destDomain)
	}

	if len(message) < 216 {
		return Event{}, fmt.Errorf("非 BurnMessage，跳過（message 長度 %d < 216）", len(message))
	}
	amount := new(big.Int).SetBytes(message[184:216]).String()

	return Event{
		Chain:       "solana",
		SourceChain: "solana",
		DestChain:   destChain,
		TxHash:      txHash,
		Nonce:       nonce,
		Amount:      amount,
		Sender:      sender,
		Recipient:   recipient,
		BlockTime:   blockTime,
		IsSource:    true,
	}, nil
}

// parseSolanaMessageReceived 解析 Borsh 編碼的 MessageReceived 事件
// struct MessageReceived { caller: Pubkey, source_domain: u32, nonce: u64, sender: [u8;32], message_body: Vec<u8> }
func parseSolanaMessageReceived(data []byte, txHash string, blockTime time.Time) (Event, error) {
	// caller: 32 bytes
	// source_domain: 4 bytes LE
	// nonce: 8 bytes LE
	// sender: 32 bytes
	// message_body: 4 bytes length + N bytes
	if len(data) < 76 {
		return Event{}, fmt.Errorf("data 太短: %d bytes", len(data))
	}

	caller := base58Encode(data[0:32])
	sourceDomain := binary.LittleEndian.Uint32(data[32:36])
	nonce := binary.LittleEndian.Uint64(data[36:44])

	sourceChain, ok := DomainToChain[sourceDomain]
	if !ok {
		sourceChain = fmt.Sprintf("domain_%d", sourceDomain)
	}

	return Event{
		Chain:       "solana",
		SourceChain: sourceChain,
		TxHash:      txHash,
		Nonce:       nonce,
		Recipient:   caller,
		BlockTime:   blockTime,
		IsSource:    false,
	}, nil
}

// base58Encode 把 bytes 轉成 base58 字串（Solana 地址格式）
func base58Encode(input []byte) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

	n := new(big.Int).SetBytes(input)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)

	var result []byte
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		result = append(result, alphabet[mod.Int64()])
	}

	// 前導零位元組 → '1'
	for _, b := range input {
		if b != 0 {
			break
		}
		result = append(result, '1')
	}

	// 反轉
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return string(result)
}

// rpcCall 送出一個 Solana JSON-RPC 請求並回傳 result 欄位的原始 JSON
func (s *SolanaIndexer) rpcCall(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.rpcURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP 請求失敗: %w", err)
	}
	defer resp.Body.Close()

	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("解析 RPC response 失敗: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC 錯誤: %s", rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}
