package db

import "time"

// TransferStatus 代表一筆跨鏈轉帳的狀態
// 就像網購訂單一樣，有「已出貨」和「已到貨」兩種狀態
type TransferStatus string

const (
	// StatusPending 代表只有來源鏈的事件，還在等目標鏈確認
	// 就像包裹已經從台灣寄出，但還沒到美國
	StatusPending TransferStatus = "pending"

	// StatusCompleted 代表兩條鏈的事件都配對成功
	// 就像包裹已經在美國簽收了
	StatusCompleted TransferStatus = "completed"
)

// Transfer 代表一筆完整的跨鏈轉帳記錄
// 這是我們存進資料庫的核心資料結構
type Transfer struct {
	// ID 是資料庫自動產生的流水號
	ID int64

	// Nonce 是 CCTP 給每筆轉帳的唯一編號，用來配對兩條鏈的事件
	// 就像快遞單號，用這個號碼可以追蹤同一筆轉帳
	Nonce uint64

	// SourceTxHash 是來源鏈上 MessageSent 的交易 hash
	SourceTxHash string

	// DestTxHash 是目的地鏈上 MessageReceived 的交易 hash
	// 剛開始是空的，等配對成功才會填入
	DestTxHash string

	// SourceChain 是來源鏈的名稱，例如 "ethereum", "solana"
	SourceChain string

	DestChain string

	// Amount 是轉帳的 USDC 金額
	Amount string

	// Sender 是 Ethereum 上的發送者錢包地址
	Sender string

	// Recipient 是 Base 上的接收者錢包地址
	Recipient string

	// SentAt 是 MessageSent 事件發生的時間
	SentAt time.Time

	// ReceivedAt 是 MessageReceived 事件發生的時間
	// 剛開始是 nil，等配對成功才會填入
	ReceivedAt *time.Time

	// DurationSeconds 是跨鏈完成花了幾秒
	// pending 的時候是 0，completed 才有值
	DurationSeconds float64

	// Status 是目前的狀態：pending 或 completed
	Status TransferStatus
}
