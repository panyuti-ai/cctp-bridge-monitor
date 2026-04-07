package main

import (
	"bufio"
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/panyuti-ai/cctp-bridge-monitor/internal/api"
	"github.com/panyuti-ai/cctp-bridge-monitor/internal/db"
	"github.com/panyuti-ai/cctp-bridge-monitor/internal/indexer"
	"github.com/panyuti-ai/cctp-bridge-monitor/internal/matcher"
	"go.uber.org/zap"
)

// loadEnv 讀取 .env 檔案並設定環境變數（已設定的不覆蓋）
func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

// evmChain 是 EVM 相容鏈的設定
// 每條鏈都同時監聽 MessageSent 和 MessageReceived，支援雙向跨鏈
type evmChain struct {
	Name         string
	RpcEnvVar    string
	ContractAddr string // CCTP MessageTransmitter 合約地址
}

// evmChains 是所有支援的 EVM 鏈
var evmChains = []evmChain{
	{
		Name:         "ethereum",
		RpcEnvVar:    "ETH_RPC_URL",
		ContractAddr: "0x0a992d191DEeC32aFe36203Ad87D7d289a738F81",
	},
	{
		Name:         "base",
		RpcEnvVar:    "BASE_RPC_URL",
		ContractAddr: "0xAD09780d193884d503182aD4588450C416D6F9D4",
	},
	{
		Name:         "avalanche",
		RpcEnvVar:    "AVAX_RPC_URL",
		ContractAddr: "0x8186359aF5F57FbB40c6b14A588d2A59C0C29880",
	},
	{
		Name:         "optimism",
		RpcEnvVar:    "OP_RPC_URL",
		ContractAddr: "0x4D41f22c5a0e5c74090899E5a8Fb597a8842b3e8",
	},
	{
		Name:         "polygon",
		RpcEnvVar:    "POLYGON_RPC_URL",
		ContractAddr: "0xF3be9355363857F3e001be68856A2f96b4C39Ba9",
	},
	{
		Name:         "arbitrum",
		RpcEnvVar:    "ARB_RPC_URL",
		ContractAddr: "0xC30362313FBBA5cf9163F0bb16a0e01f01A896ca",
	},
}

// solanaConfig 是 Solana 的設定
// Solana 用不同的 indexer（HTTP polling），所以分開設定
var solanaConfig = struct {
	RpcEnvVar string
	ProgramID string
}{
	RpcEnvVar: "SOL_RPC_URL",
	// CCTP MessageTransmitter program（mainnet）
	ProgramID: "CCTPmbSD7gX1bxKPAmg77w8oFzNFpaQiQUWD43TKaecd",
}

func main() {
	loadEnv(".env")

	logger, err := zap.NewProduction()
	if err != nil {
		panic("建立 logger 失敗: " + err.Error())
	}
	defer logger.Sync()

	database, err := db.New("cctp.db")
	if err != nil {
		logger.Fatal("建立資料庫失敗", zap.Error(err))
	}
	defer database.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 所有鏈共用同一個 channel
	eventCh := make(chan indexer.Event, 100)

	// 啟動所有 EVM 鏈的 indexer（每條鏈間隔 1 秒，避免同時建立連線觸發 rate limit）
	for i, chain := range evmChains {
		if i > 0 {
			time.Sleep(1 * time.Second)
		}
		rpcURL := os.Getenv(chain.RpcEnvVar)
		if rpcURL == "" {
			logger.Warn("沒有設定 RPC URL，跳過這條鏈",
				zap.String("chain", chain.Name),
				zap.String("envVar", chain.RpcEnvVar),
			)
			continue
		}

		var idx *indexer.Indexer
		for attempt, delay := 0, 2*time.Second; attempt < 5; attempt++ {
			var err error
			idx, err = indexer.New(chain.Name, rpcURL, chain.ContractAddr, logger)
			if err == nil {
				break
			}
			logger.Warn("建立 indexer 失敗，稍後重試",
				zap.String("chain", chain.Name),
				zap.Int("attempt", attempt+1),
				zap.Error(err),
			)
			time.Sleep(delay)
			delay *= 2
		}
		if idx == nil {
			logger.Error("建立 indexer 失敗，跳過此鏈", zap.String("chain", chain.Name))
			continue
		}

		checkpoint, err := database.GetCheckpoint(chain.Name)
		if err != nil {
			logger.Error("取得 checkpoint 失敗", zap.String("chain", chain.Name), zap.Error(err))
		}

		go idx.Start(ctx, checkpoint, eventCh)
	}

	// 啟動 Solana indexer（如果有設定 RPC URL）
	if solRPC := os.Getenv(solanaConfig.RpcEnvVar); solRPC != "" {
		solIdx := indexer.NewSolana(solRPC, solanaConfig.ProgramID, logger)
		lastSig, err := database.GetSolanaCheckpoint()
		if err != nil {
			logger.Error("取得 solana checkpoint 失敗", zap.Error(err))
		}
		go solIdx.Start(ctx, lastSig, eventCh, database.SaveSolanaCheckpoint)
	} else {
		logger.Warn("沒有設定 SOL_RPC_URL，跳過 Solana 監聽")
	}

	m := matcher.New(database, logger)
	go m.Start(ctx, eventCh)

	server := api.New(database, logger)
	go func() {
		if err := server.Start(":8080"); err != nil {
			logger.Error("HTTP server 錯誤", zap.Error(err))
		}
	}()

	logger.Info("CCTP Bridge Monitor 啟動成功，監聽 :8080")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("收到停止信號，正在關閉...")
	cancel()
	logger.Info("關閉完成")
}
