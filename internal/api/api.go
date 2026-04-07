package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/panyuti-ai/cctp-bridge-monitor/internal/db"
	"go.uber.org/zap"
)

// Server 是我們的 HTTP server 結構
// 把 gin.Engine 和 db 包在一起，讓每個 handler 都能存取資料庫
type Server struct {
	// router 是 gin 的路由器，負責把 URL 對應到對應的 handler function
	router *gin.Engine

	// db 是資料庫操作的介面
	db *db.DB

	// logger 是 zap 的 logger
	logger *zap.Logger
}

// New 建立一個新的 HTTP server 並註冊所有路由
func New(database *db.DB, logger *zap.Logger) *Server {
	gin.SetMode(gin.ReleaseMode)

	// gin.New() 建立一個乾淨的 router，不帶任何預設 middleware
	// 跟 gin.Default() 的差別是我們自己決定要加哪些 middleware
	router := gin.New()
	// 不信任任何 proxy header（X-Forwarded-For 等），避免 IP 偽造
	router.SetTrustedProxies(nil)

	s := &Server{
		router: router,
		db:     database,
		logger: logger,
	}

	// 加入 middleware
	// Recovery middleware：如果某個 handler 發生 panic，自動恢復並回傳 500
	// 就像安全網，防止一個請求的錯誤把整個 server 搞掛
	router.Use(gin.Recovery())

	// Logger middleware：每個請求都記錄 method、路徑、狀態碼、耗時
	// 就像門口的警衛，記錄每個進來的人
	router.Use(gin.Logger())

	// 註冊 API 路由
	// 所有 API 都放在 /api 這個群組下面
	api := router.Group("/api")
	{
		// GET /api/transfers 取得所有轉帳記錄
		api.GET("/transfers", s.getTransfers)

		// GET /api/transfers/:nonce 取得單筆轉帳記錄
		api.GET("/transfers/:nonce", s.getTransferByNonce)

		// GET /api/stats 取得統計數據
		api.GET("/stats", s.getStats)
	}

	// NoRoute 是 gin 的 404 handler
	// 當所有路由都找不到匹配時，就回傳 index.html
	// 這樣直接訪問 / 也能看到 dashboard
	router.NoRoute(func(c *gin.Context) {
		c.File("./web/index.html")
	})

	return s
}

// Start 啟動 HTTP server，開始監聽指定的 port
// addr 例如 ":8080"
func (s *Server) Start(addr string) error {
	s.logger.Info("HTTP server 啟動", zap.String("addr", addr))
	return s.router.Run(addr)
}

// getTransfers 處理 GET /api/transfers
// 回傳所有轉帳記錄的 JSON 列表
func (s *Server) getTransfers(c *gin.Context) {
	transfers, err := s.db.GetAllTransfers()
	if err != nil {
		s.logger.Error("GetAllTransfers 失敗", zap.Error(err))
		// c.JSON 是 gin 的回傳 JSON 方法
		// 第一個參數是 HTTP 狀態碼，第二個是要回傳的資料
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "取得資料失敗",
		})
		return
	}

	// gin.H 是 map[string]interface{} 的縮寫
	// 就是一個 key-value 的 map，方便組 JSON
	c.JSON(http.StatusOK, gin.H{
		"data":  transfers,
		"count": len(transfers),
	})
}

// getTransferByNonce 處理 GET /api/transfers/:nonce
// 回傳單筆轉帳記錄
func (s *Server) getTransferByNonce(c *gin.Context) {
	// c.Param 取出 URL 裡的參數
	// 例如 /api/transfers/12345，nonce 就是 "12345"
	nonceStr := c.Param("nonce")

	transfer, err := s.db.GetTransferByNonce(nonceStr)
	if err != nil {
		s.logger.Error("GetTransferByNonce 失敗",
			zap.String("nonce", nonceStr),
			zap.Error(err),
		)
		c.JSON(http.StatusNotFound, gin.H{
			"error": "找不到這筆轉帳記錄",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": transfer,
	})
}

// getStats 處理 GET /api/stats
// 回傳統計數據：總數、完成數、pending 數、平均完成時間
func (s *Server) getStats(c *gin.Context) {
	stats, err := s.db.GetStats()
	if err != nil {
		s.logger.Error("GetStats 失敗", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "取得統計資料失敗",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": stats,
	})
}
