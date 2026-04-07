package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// DB 是我們自己包裝的資料庫結構
// 把 sql.DB 包起來，讓外面的程式碼不需要直接碰資料庫細節
type DB struct {
	conn *sql.DB
}

// New 建立一個新的資料庫連線並初始化資料表
// 就像開店之前先把桌椅擺好
func New(path string) (*DB, error) {
	// 打開 SQLite 資料庫檔案，如果不存在就自動建立
	conn, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("開啟資料庫失敗: %w", err)
	}

	// 測試連線是否正常
	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("資料庫連線測試失敗: %w", err)
	}

	db := &DB{conn: conn}

	// 建立資料表
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("建立資料表失敗: %w", err)
	}

	return db, nil
}

// migrate 建立所有需要的資料表
// 就像蓋房子之前先打地基
func (db *DB) migrate() error {
	query := `
CREATE TABLE IF NOT EXISTS transfers (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    nonce            INTEGER NOT NULL,
    source_chain     TEXT NOT NULL DEFAULT '',
    source_tx_hash   TEXT NOT NULL,
    dest_tx_hash     TEXT DEFAULT '',
    dest_chain       TEXT DEFAULT '',
    amount           TEXT NOT NULL,
    sender           TEXT NOT NULL,
    recipient        TEXT DEFAULT '',
    sent_at          DATETIME NOT NULL,
    received_at      DATETIME,
    duration_seconds REAL DEFAULT 0,
    status           TEXT DEFAULT 'pending',
    UNIQUE(nonce, source_chain)
);

CREATE TABLE IF NOT EXISTS checkpoints (
    chain        TEXT PRIMARY KEY,
    block_number INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_nonce ON transfers(nonce);
CREATE INDEX IF NOT EXISTS idx_status ON transfers(status);

CREATE TABLE IF NOT EXISTS solana_checkpoint (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    last_sig    TEXT NOT NULL
);
`

	_, err := db.conn.Exec(query)
	return err
}

// InsertPending 插入一筆新的 pending 轉帳記錄
func (db *DB) InsertPending(nonce uint64, sourceTxHash, amount, sender, sourceChain, destChain string, sentAt time.Time) error {
	query := `
    INSERT OR IGNORE INTO transfers (nonce, source_chain, dest_chain, source_tx_hash, amount, sender, sent_at, status)
    VALUES (?, ?, ?, ?, ?, ?, ?, 'pending')
    `
	_, err := db.conn.Exec(query, nonce, sourceChain, destChain, sourceTxHash, amount, sender, sentAt)
	return err
}

// CompletePending 把一筆 pending 的記錄更新為 completed
func (db *DB) CompletePending(nonce uint64, sourceChain, destTxHash, recipient, destChain string, receivedAt time.Time) error {
	var sentAt time.Time
	err := db.conn.QueryRow(
		"SELECT sent_at FROM transfers WHERE nonce = ? AND source_chain = ?", nonce, sourceChain,
	).Scan(&sentAt)
	if err != nil {
		return fmt.Errorf("找不到 nonce %d source_chain %s 的記錄: %w", nonce, sourceChain, err)
	}

	duration := receivedAt.Sub(sentAt).Seconds()

	query := `
UPDATE transfers
SET dest_tx_hash = ?, recipient = ?, received_at = ?, duration_seconds = ?, dest_chain = ?, status = 'completed'
WHERE nonce = ? AND source_chain = ? AND status = 'pending'
`
	_, err = db.conn.Exec(query, destTxHash, recipient, receivedAt, duration, destChain, nonce, sourceChain)
	return err
}

// GetAllTransfers 取得所有轉帳記錄，最新的排在前面
// 給 dashboard 用來顯示列表
// GetAllTransfers 取得所有轉帳記錄，最新的排在前面
// 給 dashboard 用來顯示列表
func (db *DB) GetAllTransfers() ([]Transfer, error) {
	query := `
	SELECT id, nonce, source_chain, source_tx_hash, dest_tx_hash, dest_chain, amount, sender, recipient,
	       sent_at, received_at, duration_seconds, status
	FROM transfers
	ORDER BY sent_at DESC
	`
	rows, err := db.conn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var transfers []Transfer
	for rows.Next() {
		var t Transfer
		var receivedAt sql.NullTime

		err := rows.Scan(
			&t.ID, &t.Nonce, &t.SourceChain, &t.SourceTxHash, &t.DestTxHash, &t.DestChain,
			&t.Amount, &t.Sender, &t.Recipient,
			&t.SentAt, &receivedAt, &t.DurationSeconds, &t.Status,
		)
		if err != nil {
			return nil, err
		}

		if receivedAt.Valid {
			t.ReceivedAt = &receivedAt.Time
		}

		transfers = append(transfers, t)
	}
	return transfers, nil
}
func (db *DB) GetTransferByNonce(nonceStr string) (*Transfer, error) {
	query := `
	SELECT id, nonce, source_chain, source_tx_hash, dest_tx_hash, dest_chain, amount, sender, recipient,
	       sent_at, received_at, duration_seconds, status
	FROM transfers
	WHERE nonce = ?
	`
	var t Transfer
	var receivedAt sql.NullTime

	err := db.conn.QueryRow(query, nonceStr).Scan(
		&t.ID, &t.Nonce, &t.SourceChain, &t.SourceTxHash, &t.DestTxHash, &t.DestChain,
		&t.Amount, &t.Sender, &t.Recipient,
		&t.SentAt, &receivedAt, &t.DurationSeconds, &t.Status,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("nonce %s 找不到", nonceStr)
	}
	if err != nil {
		return nil, err
	}

	if receivedAt.Valid {
		t.ReceivedAt = &receivedAt.Time
	}

	return &t, nil
}

// GetStats 取得統計數據，給 dashboard 的統計區塊用
func (db *DB) GetStats() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	query := `
    SELECT
        COUNT(*) as total,
        SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END) as completed,
        AVG(CASE WHEN status = 'completed' THEN duration_seconds ELSE NULL END) as avg_duration
    FROM transfers
    `

	var total sql.NullInt64
	var completed sql.NullInt64
	var avgDuration sql.NullFloat64

	err := db.conn.QueryRow(query).Scan(&total, &completed, &avgDuration)
	if err != nil {
		return nil, err
	}

	stats["total"] = total.Int64
	stats["completed"] = completed.Int64
	stats["pending"] = total.Int64 - completed.Int64

	if avgDuration.Valid {
		stats["avg_duration_seconds"] = avgDuration.Float64
	} else {
		stats["avg_duration_seconds"] = 0
	}

	return stats, nil
}

// SaveCheckpoint 儲存某條鏈監聽到的最新 block 號碼
// 這樣程式重啟後可以從上次停下來的地方繼續，不用從頭掃
func (db *DB) SaveCheckpoint(chain string, blockNumber uint64) error {
	query := `
    INSERT INTO checkpoints (chain, block_number)
    VALUES (?, ?)
    ON CONFLICT(chain) DO UPDATE SET block_number = excluded.block_number
    `
	// ON CONFLICT DO UPDATE 是 SQLite 的 upsert 語法
	// 意思是：如果這個 chain 已經有記錄就更新，沒有就新增
	_, err := db.conn.Exec(query, chain, blockNumber)
	return err
}

// GetCheckpoint 取得某條鏈上次儲存的 block 號碼
// 如果是第一次跑，回傳 0
func (db *DB) GetCheckpoint(chain string) (uint64, error) {
	var blockNumber uint64
	err := db.conn.QueryRow(
		"SELECT block_number FROM checkpoints WHERE chain = ?", chain,
	).Scan(&blockNumber)

	// 如果查不到（第一次跑），回傳 0 不報錯
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return blockNumber, err
}

// SaveSolanaCheckpoint 儲存 Solana 上次看到的最新 signature
func (db *DB) SaveSolanaCheckpoint(sig string) error {
	_, err := db.conn.Exec(`
		INSERT INTO solana_checkpoint (id, last_sig) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET last_sig = excluded.last_sig
	`, sig)
	return err
}

// GetSolanaCheckpoint 取得 Solana 上次儲存的 signature，沒有則回傳空字串
func (db *DB) GetSolanaCheckpoint() (string, error) {
	var sig string
	err := db.conn.QueryRow("SELECT last_sig FROM solana_checkpoint WHERE id = 1").Scan(&sig)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sig, err
}

// Close 關閉資料庫連線
// 程式結束前要記得呼叫這個
func (db *DB) Close() error {
	return db.conn.Close()
}
