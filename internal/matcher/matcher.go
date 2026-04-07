package matcher

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/panyuti-ai/cctp-bridge-monitor/internal/db"
	"github.com/panyuti-ai/cctp-bridge-monitor/internal/indexer"
	"go.uber.org/zap"
)

// pendingKey 是暫存 MessageReceived 的複合 key
// nonce 在不同來源鏈之間不唯一，所以必須加上 sourceChain
type pendingKey struct {
	nonce       uint64
	sourceChain string
}

// pendingEntry 暫存還沒配對到 MessageSent 的 MessageReceived 事件
type pendingEntry struct {
	event     indexer.Event
	createdAt time.Time
}

// Matcher 負責接收所有鏈的事件，把同一筆轉帳的兩個事件配對起來
type Matcher struct {
	db              *db.DB
	logger          *zap.Logger
	pendingReceived map[pendingKey]pendingEntry
}

// New 建立一個新的 Matcher
func New(database *db.DB, logger *zap.Logger) *Matcher {
	return &Matcher{
		db:              database,
		logger:          logger,
		pendingReceived: make(map[pendingKey]pendingEntry),
	}
}

// Start 開始處理事件，同時啟動定期重試
func (m *Matcher) Start(ctx context.Context, eventCh <-chan indexer.Event) {
	m.logger.Info("Matcher started")

	// 每 30 秒掃一次暫存區，對還沒配對的 MessageReceived 重試
	// 處理程式重啟後 MessageSent 已在 DB 但 buffer 是空的情況
	retryTicker := time.NewTicker(30 * time.Second)
	defer retryTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("Matcher stopped")
			return

		case event := <-eventCh:
			if event.IsSource {
				m.handleSent(event)
			} else {
				m.handleReceived(event)
			}

		case <-retryTicker.C:
			m.retryAll()
		}
	}
}

// handleSent 處理 MessageSent 事件
func (m *Matcher) handleSent(event indexer.Event) {
	// 忽略目的地為 Noble 或未知 domain 的轉帳（無法監聽）
	if event.DestChain == "noble" || strings.HasPrefix(event.DestChain, "domain_") {
		return
	}

	m.logger.Info("MessageSent",
		zap.Uint64("nonce", event.Nonce),
		zap.String("chain", event.Chain),
		zap.String("txHash", event.TxHash),
		zap.String("amount", event.Amount),
	)

	err := m.db.InsertPending(
		event.Nonce,
		event.TxHash,
		event.Amount,
		event.Sender,
		event.Chain,
		event.DestChain,
		event.BlockTime,
	)
	if err != nil {
		m.logger.Error("InsertPending failed",
			zap.Uint64("nonce", event.Nonce),
			zap.Error(err),
		)
		return
	}

	// 插入後立刻檢查暫存區，處理 MessageReceived 比 MessageSent 先到的情況
	m.tryMatch(pendingKey{nonce: event.Nonce, sourceChain: event.Chain})

	if err := m.db.SaveCheckpoint(event.Chain, event.BlockNumber); err != nil {
		m.logger.Error("SaveCheckpoint failed", zap.Error(err))
	}
}

// handleReceived 處理 MessageReceived 事件
func (m *Matcher) handleReceived(event indexer.Event) {
	// 忽略不支援的來源鏈（未知 domain 或 Noble — Alchemy 不支援 Noble 節點）
	if strings.HasPrefix(event.SourceChain, "domain_") || event.SourceChain == "noble" {
		return
	}

	m.logger.Info("MessageReceived",
		zap.Uint64("nonce", event.Nonce),
		zap.String("destChain", event.Chain),
		zap.String("sourceChain", event.SourceChain),
		zap.String("txHash", event.TxHash),
	)

	if err := m.complete(event); err != nil {
		m.logger.Warn("match failed, queuing for retry",
			zap.Uint64("nonce", event.Nonce),
			zap.String("sourceChain", event.SourceChain),
			zap.Error(err),
		)
		key := pendingKey{nonce: event.Nonce, sourceChain: event.SourceChain}
		m.pendingReceived[key] = pendingEntry{event: event, createdAt: time.Now()}
		return
	}

	if err := m.db.SaveCheckpoint(event.Chain, event.BlockNumber); err != nil {
		m.logger.Error("SaveCheckpoint failed", zap.Error(err))
	}
}

// complete 嘗試把一個 MessageReceived 事件寫進 DB
func (m *Matcher) complete(event indexer.Event) error {
	return m.db.CompletePending(
		event.Nonce,
		event.SourceChain,
		event.TxHash,
		event.Recipient,
		event.Chain,
		event.BlockTime,
	)
}

// tryMatch 檢查暫存區有沒有可以配對的 MessageReceived，有就重試
func (m *Matcher) tryMatch(key pendingKey) {
	entry, exists := m.pendingReceived[key]
	if !exists {
		return
	}

	m.logger.Info("retrying buffered MessageReceived",
		zap.Uint64("nonce", key.nonce),
		zap.String("sourceChain", key.sourceChain),
	)

	delete(m.pendingReceived, key)
	m.handleReceived(entry.event)
}

// retryAll 對暫存區所有事件重試一次
// 處理：程式重啟後 MessageSent 已在 DB，但 buffer 是空的 → MessageReceived 卡住
func (m *Matcher) retryAll() {
	if len(m.pendingReceived) == 0 {
		return
	}

	m.logger.Info("retrying all buffered MessageReceived", zap.Int("count", len(m.pendingReceived)))

	// 複製 keys 避免在迭代中修改 map
	keys := make([]pendingKey, 0, len(m.pendingReceived))
	for k := range m.pendingReceived {
		keys = append(keys, k)
	}

	for _, key := range keys {
		entry := m.pendingReceived[key]

		// 超過 2 小時還沒配對就丟棄，避免 map 無限增長
		if time.Since(entry.createdAt) > 2*time.Hour {
			m.logger.Warn("dropping stale pending MessageReceived",
				zap.String("key", fmt.Sprintf("%s/%d", key.sourceChain, key.nonce)),
			)
			delete(m.pendingReceived, key)
			continue
		}

		if err := m.complete(entry.event); err == nil {
			m.logger.Info("retry matched",
				zap.Uint64("nonce", key.nonce),
				zap.String("sourceChain", key.sourceChain),
			)
			delete(m.pendingReceived, key)
			if err := m.db.SaveCheckpoint(entry.event.Chain, entry.event.BlockNumber); err != nil {
				m.logger.Error("SaveCheckpoint failed", zap.Error(err))
			}
		}
	}
}
