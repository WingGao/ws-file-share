package command

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"wsdownloader/internal/config"
)

func TestCLIInitAddIdempotentAndRemoveOffline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"init", "--config", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatalf("init should create only the configuration: %+v %v", entries, err)
	}
	if err := Run([]string{"init", "--config", path}, &stdout, &stderr); err == nil {
		t.Fatal("init overwrote existing configuration")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AdminListen = "127.0.0.1:1"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "带空格 file.txt")
	if err := os.WriteFile(file, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		stdout.Reset()
		if err := Run([]string{"add", file, "--config", path}, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		var resource config.Resource
		if err := json.Unmarshal(stdout.Bytes(), &resource); err != nil || resource.ID != config.ID(file) {
			t.Fatalf("add output: %s %v", stdout.String(), err)
		}
		assertPreparedHash(t, path, file, []byte("hello"))
	}
	cfg, err = config.Load(path)
	if err != nil || len(cfg.Resources) != 1 {
		t.Fatalf("duplicate add not idempotent: %+v %v", cfg, err)
	}
	if err := Run([]string{"remove", config.ID(file), "--config", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil || len(cfg.Resources) != 0 {
		t.Fatal("remove not persisted")
	}
}

func assertPreparedHash(t *testing.T, configPath, file string, content []byte) {
	t.Helper()
	db, err := bolt.Open(filepath.Join(filepath.Dir(configPath), "sha256.kv"), 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("CLI did not create a readable SHA cache: %v", err)
	}
	defer db.Close()
	real, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		var record struct {
			SHA256 string `json:"sha256"`
		}
		bucket := tx.Bucket([]byte("file-sha256-v1"))
		if bucket == nil {
			t.Fatal("CLI did not create the SHA bucket")
		}
		if err := json.Unmarshal(bucket.Get([]byte(real)), &record); err != nil {
			return err
		}
		want := sha256.Sum256(content)
		if record.SHA256 != hex.EncodeToString(want[:]) {
			t.Fatalf("CLI cached the wrong SHA: %s", record.SHA256)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentConfigurationUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			unlock, err := config.Lock(path)
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			cfg, err := config.Load(path)
			if err != nil {
				t.Error(err)
				return
			}
			p := filepath.Join(filepath.Dir(path), string(rune('a'+i)))
			cfg.Resources = append(cfg.Resources, config.Resource{ID: config.ID(p), Path: p})
			if err := config.Save(path, cfg); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	cfg, err := config.Load(path)
	if err != nil || len(cfg.Resources) != 6 {
		t.Fatalf("lost configuration update: %d %v", len(cfg.Resources), err)
	}
}

func TestCLIReloadOverHTTPWithoutCertificates(t *testing.T) {
	var called atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/reload" {
			t.Errorf("unexpected management request: %s %s", r.Method, r.URL.Path)
		}
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()
	cfg := config.Default()
	cfg.AdminListen = strings.TrimPrefix(s.URL, "http://")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"reload", "--config", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !called.Load() {
		t.Fatal("management API was not called")
	}
	called.Store(false)
	cfg.AdminListen = "127.0.0.1:1"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"reload", "--config", path, "--server", s.URL}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !called.Load() {
		t.Fatal("temporary --server override was not used")
	}
}
