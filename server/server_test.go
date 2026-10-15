package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	t.Cleanup(func() { app.Close(); s.Close() })
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
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != header.SHA256 || int64(len(data)) != header.Length {
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
	resp, err := s.Client().Post(s.URL+"/api/reload", "application/json", nil)
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
	resp, err = s.Client().Post(s.URL+"/api/reload", "application/json", nil)
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
	m, err := buildManifest(context.Background(), config.Resource{ID: config.ID(root), Path: root}, config.Default())
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
	_, s, _, _, _ := fixture(t)
	resp, err := s.Client().Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Contains(b, []byte("WS下载器")) || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatal("embedded HTML not served")
	}
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/reload", nil)
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
