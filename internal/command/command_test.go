package command

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

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
	cfg.ServerURL = "http://127.0.0.1:1"
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
	cfg.ServerURL = s.URL
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
}
