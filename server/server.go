package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"wsdownloader/client"
	"wsdownloader/internal/config"
)

type App struct {
	configPath string
	cfg        atomic.Pointer[config.Config]
	ctx        context.Context
	cancel     context.CancelFunc
	reloadMu   sync.Mutex
}

func New(configPath string, cfg config.Config) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{configPath: configPath, ctx: ctx, cancel: cancel}
	a.cfg.Store(&cfg)
	return a, nil
}

func (a *App) Close() { a.cancel() }

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(client.HTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"name": "WS下载器", "status": "ok"})
	})
	mux.HandleFunc("POST /api/reload", a.reload)
	mux.HandleFunc("GET /ws", a.serveWS)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *App) reload(w http.ResponseWriter, r *http.Request) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if !a.allowedOrigin(r, *a.cfg.Load()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "不允许的 Origin"})
		return
	}
	cfg, err := config.Load(a.configPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	old := a.cfg.Load()
	if cfg.Listen != old.Listen {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "监听地址变化需要重启服务；旧配置继续生效"})
		return
	}
	a.cfg.Store(&cfg)
	writeJSON(w, http.StatusOK, map[string]any{"status": "reloaded", "resources": len(cfg.Resources)})
}

func (a *App) allowedOrigin(r *http.Request, cfg config.Config) bool {
	origin := r.Header.Get("Origin")
	if origin == "" { // Non-browser clients, including the management CLI.
		return true
	}
	u, err := url.Parse(origin)
	// Reverse proxies preserve the external Host while terminating TLS. Do not
	// infer the browser's scheme from this application's plaintext connection.
	if err == nil && (u.Scheme == "https" || u.Scheme == "http") && strings.EqualFold(u.Host, r.Host) && u.Path == "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		return true
	}
	for _, allowed := range cfg.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

func (a *App) Serve(ctx context.Context) error {
	cfg := a.cfg.Load()
	httpServer := &http.Server{Addr: cfg.Listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 * 1024}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			a.Close() // http.Shutdown alone doesn't close upgraded connections.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(shutdownCtx)
		case <-done:
		}
	}()
	slog.Info("WS下载器启动", "listen", cfg.Listen, "url", cfg.ServerURL, "resources", len(cfg.Resources))
	err := httpServer.ListenAndServe()
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type request struct {
	Type       string `json:"type"`
	RequestID  string `json:"request_id"`
	ResourceID string `json:"resource_id"`
	FileID     string `json:"file_id"`
	Index      int64  `json:"index"`
}

type blockHeader struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	FileID    string `json:"file_id"`
	Index     int64  `json:"index"`
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
	SHA256    string `json:"sha256"`
}

type response struct {
	id     string
	json   any
	header *blockHeader
	data   []byte
	bytes  int64
}

type channel struct {
	conn     *websocket.Conn
	cfg      config.Config
	ctx      context.Context
	cancel   context.CancelFunc
	jobs     chan request
	out      chan response
	budget   *byteBudget
	mu       sync.Mutex
	active   map[string]bool
	manifest *Manifest
	listed   bool
}

func (a *App) serveWS(w http.ResponseWriter, r *http.Request) {
	cfg := *a.cfg.Load() // A connected download keeps its configuration snapshot.
	if !a.allowedOrigin(r, cfg) {
		http.Error(w, "不允许的 Origin", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 * 1024)
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	c := &channel{conn: conn, cfg: cfg, ctx: ctx, cancel: cancel, jobs: make(chan request, cfg.PendingRequests),
		out: make(chan response, cfg.Workers+1), budget: newBudget(cfg.ChannelBytes), active: map[string]bool{}}
	var workers sync.WaitGroup
	for range cfg.Workers {
		workers.Go(c.worker)
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writeLoop()
		cancel()
	}()
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, stop := context.WithTimeout(ctx, 20*time.Second)
				err := conn.Ping(pingCtx)
				stop()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var req request
		if typ != websocket.MessageText || json.Unmarshal(data, &req) != nil || req.RequestID == "" || len(req.RequestID) > 64 {
			_ = conn.Close(websocket.StatusPolicyViolation, "expected JSON request with request_id")
			break
		}
		c.mu.Lock()
		duplicate := c.active[req.RequestID]
		full := len(c.active) >= cfg.PendingRequests
		if !duplicate && !full {
			c.active[req.RequestID] = true
		}
		c.mu.Unlock()
		if duplicate || full {
			_ = conn.Close(websocket.StatusTryAgainLater, "duplicate request ID or pending request limit exceeded")
			break
		}
		select {
		case c.jobs <- req:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	cancel()
	workers.Wait()
	<-writerDone
	<-pingDone
	// Workers release buffers not handed to the writer. Drain any queued ones.
	for {
		select {
		case result := <-c.out:
			c.budget.release(result.bytes)
		default:
			return
		}
	}
}

func (c *channel) worker() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case req := <-c.jobs:
			result := c.process(req)
			select {
			case c.out <- result:
			case <-c.ctx.Done():
				c.budget.release(result.bytes)
				return
			}
		}
	}
}

func failure(id, code string, err error) response {
	return response{id: id, json: map[string]string{"type": "error", "request_id": id, "code": code, "message": err.Error()}}
}

func (c *channel) process(req request) response {
	if req.Type == "manifest" {
		c.mu.Lock()
		listed := c.listed
		c.listed = true
		c.mu.Unlock()
		if listed {
			return failure(req.RequestID, "already_listed", errors.New("每条连接只获取一次目录树，请重新连接以下载其他资源"))
		}
		var resource *config.Resource
		for _, r := range c.cfg.Resources {
			if r.ID == req.ResourceID {
				resource = &r
				break
			}
		}
		if resource == nil {
			return failure(req.RequestID, "not_found", errors.New("资源 ID 不存在"))
		}
		m, err := buildManifest(c.ctx, *resource, c.cfg)
		if err != nil {
			return failure(req.RequestID, "manifest_failed", err)
		}
		c.mu.Lock()
		c.manifest = m
		c.mu.Unlock()
		return response{id: req.RequestID, json: map[string]any{"type": "manifest", "request_id": req.RequestID, "manifest": m}}
	}
	c.mu.Lock()
	m := c.manifest
	c.mu.Unlock()
	if req.Type != "block" {
		return failure(req.RequestID, "unknown_type", errors.New("未知请求类型"))
	}
	if m == nil {
		return failure(req.RequestID, "not_ready", errors.New("请先获取目录树"))
	}
	file, ok := m.files[req.FileID]
	if !ok {
		return failure(req.RequestID, "not_found", errors.New("文件不在当前目录树中"))
	}
	if req.Index < 0 || file.Size == 0 || req.Index > (file.Size-1)/m.ChunkBytes {
		return failure(req.RequestID, "invalid_block", errors.New("分块索引超出文件范围"))
	}
	offset := req.Index * m.ChunkBytes
	n := min(m.ChunkBytes, file.Size-offset)
	if err := c.budget.acquire(c.ctx, n); err != nil {
		return failure(req.RequestID, "cancelled", err)
	}
	data := make([]byte, int(n))
	f, err := os.Open(file.source)
	if err == nil {
		_, err = f.ReadAt(data, offset)
		_ = f.Close()
	}
	if err != nil {
		c.budget.release(n)
		return failure(req.RequestID, "read_failed", err)
	}
	sum := sha256.Sum256(data)
	return response{id: req.RequestID, bytes: n, data: data, header: &blockHeader{Type: "block", RequestID: req.RequestID,
		FileID: file.ID, Index: req.Index, Offset: offset, Length: n, SHA256: hex.EncodeToString(sum[:])}}
}

func (c *channel) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case result := <-c.out:
			ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
			err := c.writeResponse(ctx, result)
			cancel()
			c.budget.release(result.bytes)
			c.mu.Lock()
			delete(c.active, result.id)
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (c *channel) writeResponse(ctx context.Context, result response) error {
	if result.header == nil {
		data, err := json.Marshal(result.json)
		if err != nil {
			return err
		}
		return c.conn.Write(ctx, websocket.MessageText, data)
	}
	header, err := json.Marshal(result.header)
	if err != nil {
		return err
	}
	w, err := c.conn.Writer(ctx, websocket.MessageBinary)
	if err != nil {
		return err
	}
	// One binary message: uint32 big-endian JSON-header length, JSON, file bytes.
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(header)))
	if _, err = w.Write(prefix[:]); err == nil {
		_, err = w.Write(header)
	}
	// Smaller writes permit WS fragmentation without another 4 MiB copy.
	for start := 0; err == nil && start < len(result.data); start += 64 * 1024 {
		_, err = w.Write(result.data[start:min(start+64*1024, len(result.data))])
	}
	if err != nil {
		_ = c.conn.CloseNow()
		return err
	}
	return w.Close()
}
