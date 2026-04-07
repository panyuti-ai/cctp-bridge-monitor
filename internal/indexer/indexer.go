package indexer

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"go.uber.org/zap"
)

const (
	MessageSentTopic     = "0x8c5261668696ce22758910d05bab8f186d6eb247ceac2af2e82c7dc17669b036"
	MessageReceivedTopic = "0x58200b4c34ae05ee816d710053fff3fb75af4395915d3d2a771b24aa10e3cc5d"
)

// DomainToChain 把 CCTP domain ID 對應到鏈名稱
// CCTP 每條鏈有固定的 domain ID，MessageReceived 事件裡用這個標示來源鏈
var DomainToChain = map[uint32]string{
	0: "ethereum",
	1: "avalanche",
	2: "optimism",
	3: "arbitrum",
	4: "noble",
	5: "solana",
	6: "base",
	7: "polygon",
}

// Event 是我們從鏈上解析出來的事件資料
type Event struct {
	Chain       string
	SourceChain string // 只有 MessageReceived 有意義，代表發送方的鏈
	DestChain   string // 只有 MessageSent 有意義，代表目的地鏈（從 destinationDomain 轉換）
	TxHash      string
	Nonce       uint64
	Amount      string
	Sender      string
	Recipient   string
	BlockTime   time.Time
	BlockNumber uint64
	// IsSource 代表這個事件是 MessageSent（true）還是 MessageReceived（false）
	IsSource bool
}

// Indexer 負責連接一條鏈並同時監聽 MessageSent 和 MessageReceived
type Indexer struct {
	chain           string
	client          *ethclient.Client
	contractAddress common.Address
	logger          *zap.Logger
	lastBlock       uint64                  // 最後一次處理的 block，斷線重連時從這裡補掃
	headerCache     map[uint64]*types.Header // block header cache，避免同一 block N+1 RPC
}

var (
	messageSentHash     = common.HexToHash(MessageSentTopic)
	messageReceivedHash = common.HexToHash(MessageReceivedTopic)
)

// New 建立一個新的 Indexer，同時監聽 MessageSent 和 MessageReceived
func New(chain, rpcURL, contractAddr string, logger *zap.Logger) (*Indexer, error) {
	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("連接 %s 節點失敗: %w", chain, err)
	}

	return &Indexer{
		chain:           chain,
		client:          client,
		contractAddress: common.HexToAddress(contractAddr),
		logger:          logger,
		headerCache:     make(map[uint64]*types.Header),
	}, nil
}

// Start 開始監聽事件
func (idx *Indexer) Start(ctx context.Context, fromBlock uint64, eventCh chan<- Event) {
	idx.logger.Info("開始監聽事件",
		zap.String("chain", idx.chain),
		zap.Uint64("fromBlock", fromBlock),
	)

	currentBlock, err := idx.client.BlockNumber(ctx)
	if err != nil {
		idx.logger.Error("取得最新 block 失敗", zap.Error(err))
		return
	}

	if fromBlock > 0 && fromBlock < currentBlock {
		idx.logger.Info("補掃歷史 block",
			zap.Uint64("from", fromBlock),
			zap.Uint64("to", currentBlock),
		)
		idx.scanRange(ctx, fromBlock, currentBlock, eventCh)
	}
	idx.lastBlock = currentBlock

	for {
		if ctx.Err() != nil {
			return
		}
		idx.subscribe(ctx, eventCh)
		if ctx.Err() != nil {
			return
		}
		idx.logger.Info("訂閱斷開，5 秒後重連", zap.String("chain", idx.chain))
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		// 補掃斷線期間遺漏的 block
		if reconnectBlock, err := idx.client.BlockNumber(ctx); err == nil && reconnectBlock > idx.lastBlock {
			idx.logger.Info("補掃斷線期間遺漏的 block",
				zap.String("chain", idx.chain),
				zap.Uint64("from", idx.lastBlock),
				zap.Uint64("to", reconnectBlock),
			)
			idx.scanRange(ctx, idx.lastBlock, reconnectBlock, eventCh)
			idx.lastBlock = reconnectBlock
		}
	}
}

// scanRange 掃描歷史事件
func (idx *Indexer) scanRange(ctx context.Context, from, to uint64, eventCh chan<- Event) {
	defer func() { idx.headerCache = make(map[uint64]*types.Header) }()
	batchSize := uint64(10) // Alchemy 免費方案限制最多 10 blocks per eth_getLogs

	for start := from; start <= to; start += batchSize {
		if ctx.Err() != nil {
			return
		}
		end := start + batchSize - 1
		if end > to {
			end = to
		}

		query := ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(start),
			ToBlock:   new(big.Int).SetUint64(end),
			Addresses: []common.Address{idx.contractAddress},
			Topics:    [][]common.Hash{{messageSentHash, messageReceivedHash}},
		}

		// rate limit retry：最多重試 5 次，指數退避
		var logs []types.Log
		var err error
		delay := 500 * time.Millisecond
		for attempt := 0; attempt < 5; attempt++ {
			logs, err = idx.client.FilterLogs(ctx, query)
			if err == nil {
				break
			}
			idx.logger.Warn("掃描歷史 log 失敗，稍後重試",
				zap.String("chain", idx.chain),
				zap.Uint64("from", start),
				zap.Uint64("to", end),
				zap.Int("attempt", attempt+1),
				zap.Error(err),
			)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay *= 2
		}
		if err != nil {
			idx.logger.Error("掃描歷史 log 失敗，跳過此批次",
				zap.String("chain", idx.chain),
				zap.Uint64("from", start),
				zap.Uint64("to", end),
				zap.Error(err),
			)
			continue
		}

		// 批次間稍作暫停，避免觸發 rate limit
		time.Sleep(150 * time.Millisecond)

		for _, log := range logs {
			event, err := idx.parseLog(ctx, log)
			if err != nil {
				idx.logger.Warn("跳過 log (scan)",
					zap.String("chain", idx.chain),
					zap.String("topic", log.Topics[0].Hex()),
					zap.Error(err),
				)
				continue
			}
			eventCh <- event
		}
	}
}

// subscribe 訂閱即時事件
func (idx *Indexer) subscribe(ctx context.Context, eventCh chan<- Event) {
	query := ethereum.FilterQuery{
		Addresses: []common.Address{idx.contractAddress},
		Topics:    [][]common.Hash{{messageSentHash, messageReceivedHash}},
	}

	logCh := make(chan types.Log)

	sub, err := idx.client.SubscribeFilterLogs(ctx, query, logCh)
	if err != nil {
		idx.logger.Error("訂閱事件失敗", zap.Error(err))
		return
	}
	defer sub.Unsubscribe()

	idx.logger.Info("開始即時訂閱", zap.String("chain", idx.chain))

	for {
		select {
		case <-ctx.Done():
			idx.logger.Info("停止監聽", zap.String("chain", idx.chain))
			return
		case err := <-sub.Err():
			idx.logger.Error("訂閱發生錯誤", zap.Error(err))
			return
		case log := <-logCh:
			event, err := idx.parseLog(ctx, log)
			if err != nil {
				idx.logger.Warn("跳過 log (subscribe)",
					zap.String("chain", idx.chain),
					zap.String("topic", log.Topics[0].Hex()),
					zap.Error(err),
				)
				continue
			}
			if log.BlockNumber > idx.lastBlock {
				idx.lastBlock = log.BlockNumber
			}
			eventCh <- event
		}
	}
}

// parseLog 根據 topic[0] 決定解析哪種事件
func (idx *Indexer) parseLog(ctx context.Context, log types.Log) (Event, error) {
	if len(log.Topics) == 0 {
		return Event{}, fmt.Errorf("log 沒有 topics")
	}

	header, ok := idx.headerCache[log.BlockNumber]
	if !ok {
		var err error
		header, err = idx.client.HeaderByNumber(ctx, new(big.Int).SetUint64(log.BlockNumber))
		if err != nil {
			return Event{}, fmt.Errorf("取得 block header 失敗: %w", err)
		}
		idx.headerCache[log.BlockNumber] = header
	}

	blockTime := time.Unix(int64(header.Time), 0)

	switch log.Topics[0] {
	case messageSentHash:
		return idx.parseMessageSent(log, blockTime)
	case messageReceivedHash:
		return idx.parseMessageReceived(log, blockTime)
	default:
		return Event{}, fmt.Errorf("未知的 topic: %s", log.Topics[0].Hex())
	}
}

// parseMessageSent 解析 MessageSent(bytes message) 事件
// event MessageSent(bytes message)
// data = abi.encode(offset, length, message_bytes)
func (idx *Indexer) parseMessageSent(log types.Log, blockTime time.Time) (Event, error) {
	if len(log.Data) < 64 {
		return Event{}, fmt.Errorf("data 太短: %d bytes", len(log.Data))
	}

	// data[0:32]  = offset (指向 message 的起始位置，通常是 0x20)
	// data[32:64] = message 的 byte 長度
	// data[64:]   = message 內容
	message := log.Data[64:]

	if len(message) < 116 {
		return Event{}, fmt.Errorf("message 太短: %d bytes", len(message))
	}

	// CCTP message header layout:
	// [0:4]   version
	// [4:8]   sourceDomain
	// [8:12]  destinationDomain
	// [12:20] nonce
	// [20:52] sender
	// [52:84] recipient
	// [84:116] destinationCaller
	// [116:]  messageBody
	destDomain := uint32(new(big.Int).SetBytes(message[8:12]).Uint64())
	nonce := new(big.Int).SetBytes(message[12:20]).Uint64()
	sender := common.BytesToAddress(message[20:52]).Hex()
	recipient := common.BytesToAddress(message[52:84]).Hex()

	destChain, ok := DomainToChain[destDomain]
	if !ok {
		destChain = fmt.Sprintf("domain_%d", destDomain)
	}

	// messageBody layout (BurnMessage):
	// [0:4]   version
	// [4:36]  burnToken
	// [36:68] mintRecipient
	// [68:100] amount
	// message[116:] = messageBody，amount 在 messageBody[68:100] = message[184:216]
	if len(message) < 216 {
		// 不是 BurnMessage（例如 CCTP 的其他 message type），跳過
		return Event{}, fmt.Errorf("非 BurnMessage，跳過（message 長度 %d < 216）", len(message))
	}
	amount := new(big.Int).SetBytes(message[184:216]).String()

	return Event{
		Chain:       idx.chain,
		SourceChain: idx.chain,
		DestChain:   destChain,
		TxHash:      log.TxHash.Hex(),
		Nonce:       nonce,
		Amount:      amount,
		Sender:      sender,
		Recipient:   recipient,
		BlockTime:   blockTime,
		BlockNumber: log.BlockNumber,
		IsSource:    true,
	}, nil
}

// parseMessageReceived 解析 MessageReceived 事件
// event MessageReceived(address indexed caller, uint32 sourceDomain, uint64 indexed nonce, bytes32 sender, bytes messageBody)
// sourceDomain 不是 indexed，在 data[0:32]（右對齊 uint32）
func (idx *Indexer) parseMessageReceived(log types.Log, blockTime time.Time) (Event, error) {
	if len(log.Topics) < 3 {
		return Event{}, fmt.Errorf("topics 數量不足: %d（需要 3 個）", len(log.Topics))
	}
	if len(log.Data) < 32 {
		return Event{}, fmt.Errorf("data 太短: %d bytes", len(log.Data))
	}

	// Topics[1] = caller（indexed address）
	// Topics[2] = nonce（indexed uint64）
	// data[0:32]  = sourceDomain（uint32，ABI encoded）
	caller := common.HexToAddress(log.Topics[1].Hex()).Hex()
	nonce := new(big.Int).SetBytes(log.Topics[2].Bytes()).Uint64()
	sourceDomain := uint32(new(big.Int).SetBytes(log.Data[0:32]).Uint64())

	sourceChain, ok := DomainToChain[sourceDomain]
	if !ok {
		sourceChain = fmt.Sprintf("domain_%d", sourceDomain)
	}

	return Event{
		Chain:       idx.chain,
		SourceChain: sourceChain,
		TxHash:      log.TxHash.Hex(),
		Nonce:       nonce,
		Recipient:   caller,
		BlockTime:   blockTime,
		BlockNumber: log.BlockNumber,
		IsSource:    false,
	}, nil
}
