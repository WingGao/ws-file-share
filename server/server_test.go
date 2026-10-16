package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"wsdownloader/internal/config"
)

func fixture(t *testing.T) (*App, *httptest.Server, config.Config, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "资料")
	if err := os.MkdirAll(filepath.Join(root, "空目录"), 0755); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("WS下载器\x00\xff"), (9*config.MiB)/len("WS下载器\x00\xff")+1)
	if err := os.WriteFile(filepath.Join(root, "中文.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Resources = []config.Resource{{ID: config.ID(root), Path: root}}
	configPath := filepath.Join(dir, "config.yaml")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	app, err := New(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(app.Handler())
	admin := httptest.NewServer(app.AdminHandler())
	cfg.AdminListen = strings.TrimPrefix(admin.URL, "http://")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	app.cfg.Store(&cfg)
	t.Cleanup(func() { app.Close(); admin.Close(); s.Close() })
	return app, s, cfg, configPath, data
}

func dial(t *testing.T, s *httptest.Server) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, strings.Replace(s.URL, "http", "ws", 1)+"/ws", &websocket.DialOptions{HTTPClient: s.Client()})
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(8 * config.MiB)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn, ctx
}

func send(t *testing.T, ctx context.Context, c *websocket.Conn, req request) {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func getManifest(t *testing.T, ctx context.Context, conn *websocket.Conn, resourceID string) Manifest {
	t.Helper()
	send(t, ctx, conn, request{Type: "manifest", RequestID: "manifest", ResourceID: resourceID})
	typ, b, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("manifest response: type=%v err=%v", typ, err)
	}
	var result struct {
		Type     string   `json:"type"`
		Manifest Manifest `json:"manifest"`
	}
	if err := json.Unmarshal(b, &result); err != nil || result.Type != "manifest" {
		t.Fatalf("manifest response: %s (%v)", b, err)
	}
	return result.Manifest
}

func TestDownloadOutOfOrderAndTraversal(t *testing.T) {
	_, s, cfg, _, original := fixture(t)
	conn, ctx := dial(t, s)
	m := getManifest(t, ctx, conn, cfg.Resources[0].ID)
	var file Entry
	for _, e := range m.Entries {
		if strings.HasSuffix(e.Path, "中文.bin") {
			file = e
		}
	}
	if file.ID == "" || len(m.Entries) != 4 {
		t.Fatalf("unexpected tree: %+v", m.Entries)
	}
	assembled := make([]byte, file.Size)
	blocks := (file.Size + m.ChunkBytes - 1) / m.ChunkBytes
	for index := blocks - 1; index >= 0; index-- {
		send(t, ctx, conn, request{Type: "block", RequestID: "block-" + string(rune('a'+index)), FileID: file.ID, Index: index})
	}
	for range blocks {
		typ, packet, err := conn.Read(ctx)
		if err != nil || typ != websocket.MessageBinary || len(packet) < 4 {
			t.Fatalf("block response: type=%v err=%v", typ, err)
		}
		headerLength := int(binary.BigEndian.Uint32(packet[:4]))
		var header blockHeader
		if err := json.Unmarshal(packet[4:4+headerLength], &header); err != nil {
			t.Fatal(err)
		}
		data := packet[4+headerLength:]
		sum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
		if fmt.Sprintf("%08x", sum) != header.CRC32C || int64(len(data)) != header.Length {
			t.Fatal("block length or checksum mismatch")
		}
		copy(assembled[header.Offset:], data)
	}
	if !bytes.Equal(original, assembled) {
		t.Fatal("assembled file differs")
	}
	for _, req := range []request{
		{Type: "block", RequestID: "escape", FileID: "../config.yaml"},
		{Type: "block", RequestID: "negative", FileID: file.ID, Index: -1},
		{Type: "block", RequestID: "too-large", FileID: file.ID, Index: 999},
	} {
		send(t, ctx, conn, req)
		_, b, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		_ = json.Unmarshal(b, &result)
		if result["type"] != "error" || result["request_id"] != req.RequestID {
			t.Fatalf("expected scoped error, got %s", b)
		}
	}
}

func TestTLSReverseProxyToPlaintextBackend(t *testing.T) {
	_, backend, cfg, _, _ := fixture(t)
	upstream, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Only the simulated edge proxy owns TLS. The application is HTTP/WS.
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(upstream))
	t.Cleanup(proxy.Close)
	resp, err := proxy.Client().Get(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(page, []byte("WS下载器")) {
		t.Fatalf("HTTPS proxy page failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	endpoint := strings.Replace(proxy.URL, "https", "wss", 1) + "/ws"
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient: proxy.Client(), HTTPHeader: http.Header{"Origin": {proxy.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 * config.MiB)
	m := getManifest(t, ctx, conn, cfg.Resources[0].ID)
	var file Entry
	for _, entry := range m.Entries {
		if entry.Size > 0 {
			file = entry
			break
		}
	}
	send(t, ctx, conn, request{Type: "block", RequestID: "proxied-block", FileID: file.ID})
	typ, packet, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || len(packet) <= 4 {
		t.Fatalf("WSS to WS forwarding failed: %v", err)
	}
	bad, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient: proxy.Client(), HTTPHeader: http.Header{"Origin": {"https://unrelated.example"}},
	})
	if bad != nil {
		_ = bad.CloseNow()
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("unrelated browser Origin accepted through proxy")
	}
}

func TestReloadIsAtomicAndKeepsConnectedSnapshot(t *testing.T) {
	app, s, cfg, configPath, _ := fixture(t)
	conn, ctx := dial(t, s)
	m := getManifest(t, ctx, conn, cfg.Resources[0].ID)
	if err := os.WriteFile(configPath, []byte("chunk_bytes: invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	resp, err := s.Client().Post(cfg.AdminURL()+"/api/reload", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || len(app.cfg.Load().Resources) != 1 {
		t.Fatal("invalid reload replaced active configuration")
	}
	cfg.Resources = []config.Resource{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	resp, err = s.Client().Post(cfg.AdminURL()+"/api/reload", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	var file Entry
	for _, e := range m.Entries {
		if e.Size > 0 {
			file = e
		}
	}
	send(t, ctx, conn, request{Type: "block", RequestID: "old", FileID: file.ID})
	if typ, _, err := conn.Read(ctx); err != nil || typ != websocket.MessageBinary {
		t.Fatalf("old download stopped by reload: %v", err)
	}
	newConn, newCtx := dial(t, s)
	send(t, newCtx, newConn, request{Type: "manifest", RequestID: "new", ResourceID: m.ResourceID})
	_, b, err := newConn.Read(newCtx)
	if err != nil || !bytes.Contains(b, []byte(`"not_found"`)) {
		t.Fatalf("removed resource still available in new session: %s %v", b, err)
	}
}

func TestSymlinkMaterializationAndCycle(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "share")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("external target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "alias.txt")); err != nil {
		t.Skipf("OS cannot create test symlinks: %v", err)
	}
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	m, err := buildManifest(context.Background(), config.Resource{ID: config.ID(root), Path: root}, config.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := m.files["share/alias.txt"]
	if !ok || entry.Kind != "file" || entry.Size != int64(len("external target")) || entry.source != outside || len(m.Warnings) != 1 {
		t.Fatalf("unexpected symlink manifest: %+v warnings=%+v", m.Entries, m.Warnings)
	}
}

func TestByteBudgetConcurrentBoundAndCancellation(t *testing.T) {
	b := newBudget(32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 30 {
				if err := b.acquire(context.Background(), 4); err != nil {
					t.Error(err)
					return
				}
				runtime.Gosched()
				b.release(4)
			}
		})
	}
	wg.Wait()
	if b.used != 0 || b.peak > 32 || b.peak == 0 {
		t.Fatalf("invalid budget state: used=%d peak=%d", b.used, b.peak)
	}
	if err := b.acquire(context.Background(), 32); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.acquire(ctx, 4); err != context.Canceled {
		t.Fatalf("cancelled acquire: %v", err)
	}
	b.release(32)
}

func TestPageAndOriginPolicy(t *testing.T) {
	_, s, cfg, _, _ := fixture(t)
	resp, err := s.Client().Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Contains(b, []byte("WS下载器")) || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatal("embedded HTML not served")
	}
	req, _ := http.NewRequest(http.MethodPost, cfg.AdminURL()+"/api/reload", nil)
	req.Header.Set("Origin", "https://other.example")
	resp, err = s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("unexpected origin accepted")
	}
}

func TestPageClientWSURLAndReload(t *testing.T) {
	cfg := config.Default()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	app, err := New(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	handler := app.Handler()
	adminHandler := app.AdminHandler()
	for _, address := range []string{"", "wss://downloads.example.com/custom/ws", `wss://downloads.example.com/ws/"><script>alert(1)</script>`} {
		cfg.ClientWSURL = address
		if err := config.Save(configPath, cfg); err != nil {
			t.Fatal(err)
		}
		reload := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/reload", nil)
		req.RemoteAddr = "127.0.0.1:50000"
		adminHandler.ServeHTTP(reload, req)
		if reload.Code != http.StatusOK {
			t.Fatalf("reload failed: %s", reload.Body.String())
		}
		page := httptest.NewRecorder()
		handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
		want := `<meta name="ws-url" content="` + html.EscapeString(address) + `" />`
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("page missing configured WS address %q", address)
		}
		if strings.Contains(page.Body.String(), "<script>alert(1)</script>") {
			t.Fatal("WS URL escaped its HTML attribute")
		}
	}
}

func TestCustomWSPathReloadAndExistingConnection(t *testing.T) {
	for _, wsPath := range []string{"/download/socket", "/transfer/"} {
		t.Run(wsPath, func(t *testing.T) {
			_, s, cfg, configPath, _ := fixture(t)
			oldConn, ctx := dial(t, s)
			manifest := getManifest(t, ctx, oldConn, cfg.Resources[0].ID)
			cfg.WSPath = wsPath
			if err := config.Save(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			resp, err := s.Client().Post(cfg.AdminURL()+"/api/reload", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("reload failed: %s", resp.Status)
			}
			for _, route := range []string{"/ws", wsPath + "other"} {
				resp, err := s.Client().Get(s.URL + route)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("unconfigured path %q returned %s", route, resp.Status)
				}
			}
			conn, _, err := websocket.Dial(ctx, strings.Replace(s.URL, "http", "ws", 1)+wsPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			getManifest(t, ctx, conn, cfg.Resources[0].ID)
			for _, entry := range manifest.Entries {
				if entry.Kind == "file" && entry.Size > 0 {
					send(t, ctx, oldConn, request{Type: "block", RequestID: "old-path", FileID: entry.ID})
					if typ, _, err := oldConn.Read(ctx); err != nil || typ != websocket.MessageBinary {
						t.Fatalf("path reload interrupted existing connection: %v", err)
					}
					break
				}
			}
			resp, err = s.Client().Get(s.URL)
			if err != nil {
				t.Fatal(err)
			}
			page, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if !strings.Contains(string(page), `<meta name="ws-path" content="`+wsPath+`" />`) {
				t.Fatal("page still uses the previous WS path")
			}
		})
	}
}

func TestManagementAPIIsSeparateFromDownloadAndProxy(t *testing.T) {
	app, download, cfg, configPath, _ := fixture(t)
	upstream, err := url.Parse(download.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(upstream))
	defer proxy.Close()
	cfg.Resources = nil
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{download.URL, proxy.URL} {
		for _, route := range []string{"/api", "/api/reload", "/api/unknown"} {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				req, _ := http.NewRequest(method, endpoint+route, nil)
				req.Header.Set("X-Forwarded-For", "127.0.0.1")
				resp, err := download.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("download/proxy exposed %s %s: %s", method, route, resp.Status)
				}
			}
		}
	}
	if len(app.cfg.Load().Resources) != 1 {
		t.Fatal("public requests reloaded configuration")
	}
	resp, err := download.Client().Post(cfg.AdminURL()+"/api/reload", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(app.cfg.Load().Resources) != 0 {
		t.Fatal("local management endpoint did not reload configuration")
	}
	for _, route := range []string{"/", "/healthz", "/ws"} {
		resp, err := download.Client().Get(cfg.AdminURL() + route)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("management endpoint exposed download route %q: %s", route, resp.Status)
		}
	}
}

func TestManagementRejectsRemotePeersAndBrowserOrigins(t *testing.T) {
	app, _, cfg, _, _ := fixture(t)
	handler := app.AdminHandler()
	for _, tc := range []struct {
		peer, origin string
		status       int
	}{
		{"127.0.0.1:50000", "", http.StatusOK},
		{"[::1]:50000", "", http.StatusOK},
		{"203.0.113.10:50000", "", http.StatusForbidden},
		{"192.168.1.10:50000", "", http.StatusForbidden},
		{"invalid", "", http.StatusForbidden},
		{"127.0.0.1:50000", cfg.AdminURL(), http.StatusForbidden},
		{"127.0.0.1:50000", "https://external.example", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodPost, cfg.AdminURL()+"/api/reload", nil)
		req.RemoteAddr = tc.peer
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		req.Header.Set("Origin", tc.origin)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != tc.status {
			t.Errorf("peer=%q origin=%q: HTTP %d, want %d", tc.peer, tc.origin, result.Code, tc.status)
		}
	}
}

func TestAdminListenChangeRequiresRestart(t *testing.T) {
	app, s, cfg, configPath, _ := fixture(t)
	old := cfg.AdminListen
	endpoint := cfg.AdminURL()
	cfg.AdminListen = "127.0.0.1:8082"
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	resp, err := s.Client().Post(endpoint+"/api/reload", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || app.cfg.Load().AdminListen != old {
		t.Fatal("management listen address changed during reload")
	}
}

func TestServeSeparateListenersAndShutdown(t *testing.T) {
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer public.Close()
	admin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	cfg := config.Default()
	cfg.Listen, cfg.AdminListen = public.Addr().String(), admin.Addr().String()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	app, err := New(configPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	var serveErr error
	_ = public.Close()
	_ = admin.Close()
	go func() {
		serveErr = app.Serve(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(7 * time.Second):
			t.Error("servers did not shut down")
		}
	})
	client := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(5 * time.Second); ; {
		resp, err := client.Get("http://" + cfg.Listen + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-stopped:
			t.Fatalf("server stopped during startup: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server startup timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, tc := range []struct {
		endpoint string
		status   int
	}{
		{"http://" + cfg.Listen, http.StatusNotFound},
		{cfg.AdminURL(), http.StatusOK},
	} {
		resp, err := client.Post(tc.endpoint+"/api/reload", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s reload: %s", tc.endpoint, resp.Status)
		}
	}
	cancel()
	select {
	case <-stopped:
		if serveErr != nil {
			t.Fatal(serveErr)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("shutdown timed out")
	}
	for _, address := range []string{cfg.Listen, cfg.AdminListen} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("listener leaked after shutdown: %s: %v", address, err)
		}
		_ = listener.Close()
	}
}

func TestAdminBindFailureReleasesDownloadListener(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Listen, cfg.AdminListen = free.Addr().String(), occupied.Addr().String()
	_ = free.Close()
	app, err := New(filepath.Join(t.TempDir(), "config.yaml"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err := app.Serve(context.Background()); err == nil || !strings.Contains(err.Error(), "管理端口") {
		t.Fatalf("expected management bind failure: %v", err)
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatalf("download listener leaked after startup failure: %v", err)
	}
	_ = listener.Close()
}

func TestReloadPreparesHashesBeforeBrowserManifest(t *testing.T) {
	app, s, cfg, _, _ := fixture(t)
	var reads atomic.Int32
	app.hashes.readHash = func(ctx context.Context, path string, info os.FileInfo) (string, error) {
		reads.Add(1)
		return readFileHash(ctx, path, info)
	}
	for range 2 {
		body, _ := json.Marshal(map[string]string{"prepare_resource_id": cfg.Resources[0].ID})
		resp, err := s.Client().Post(cfg.AdminURL()+"/api/reload", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Prepared Preparation `json:"prepared"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || result.Prepared.Files != 2 || result.Prepared.Warnings != 0 {
			t.Fatalf("preparation failed: %+v, %s, %v", result, resp.Status, err)
		}
	}
	conn, ctx := dial(t, s)
	getManifest(t, ctx, conn, cfg.Resources[0].ID)
	if reads.Load() != 2 {
		t.Fatalf("prepared files were read %d times instead of 2", reads.Load())
	}
	resp, err := s.Client().Post(cfg.AdminURL()+"/api/reload", "application/json", strings.NewReader(`{"prepare_resource_id":"unknown"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal("preparation accepted an unconfigured resource")
	}
}
