package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestClientWSURLConfiguration(t *testing.T) {
	for _, address := range []string{"", "ws://127.0.0.1:8080", "wss://downloads.example.com/ws", "wss://downloads.example.com/custom/socket"} {
		t.Run(address, func(t *testing.T) {
			cfg := Default()
			cfg.ClientWSURL = address
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := Save(path, cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := Load(path)
			if err != nil || loaded.ClientWSURL != address {
				t.Fatalf("client_ws_url round trip: %q, %v", loaded.ClientWSURL, err)
			}
		})
	}
	for _, address := range []string{"https://example.com/ws", "/ws", "ws://", "wss://user:password@example.com/ws", "wss://example.com/ws?key=value", "wss://example.com/ws?", "wss://example.com/ws#fragment"} {
		cfg := Default()
		cfg.ClientWSURL = address
		if err := cfg.Validate(); err == nil {
			t.Errorf("invalid client_ws_url accepted: %q", address)
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:8080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.ClientWSURL != "" || loaded.WSPath != "/ws" {
		t.Fatalf("configuration without client_ws_url: %q, %v", loaded.ClientWSURL, err)
	}
}

func TestWSPathConfiguration(t *testing.T) {
	for _, wsPath := range []string{"/ws", "/download/socket", "/transfer/"} {
		cfg := Default()
		cfg.WSPath = wsPath
		configPath := filepath.Join(t.TempDir(), "config.yaml")
		if err := Save(configPath, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(configPath)
		if err != nil || loaded.WSPath != wsPath {
			t.Fatalf("ws_path round trip: %q, %v", loaded.WSPath, err)
		}
	}
	for _, wsPath := range []string{"", "/", "ws", "//example.com/socket", "/healthz", "/api", "/api/reload", "/api/socket", "/socket?key=value", "/socket#fragment", "/socket/../ws", "/socket//ws", `/socket\ws`, "/socket%2fws"} {
		cfg := Default()
		cfg.WSPath = wsPath
		if err := cfg.Validate(); err == nil {
			t.Errorf("invalid ws_path accepted: %q", wsPath)
		}
	}
}

func TestAdminListenIsLoopbackOnly(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8081", "localhost:8081", "[::1]:8081", "127.0.0.2:8081", "127.0.0.1:0"} {
		cfg := Default()
		cfg.AdminListen = address
		configPath := filepath.Join(t.TempDir(), "config.yaml")
		if err := Save(configPath, cfg); err != nil {
			t.Fatalf("valid admin_listen %q: %v", address, err)
		}
		loaded, err := Load(configPath)
		if err != nil || loaded.AdminListen != address {
			t.Fatalf("admin_listen round trip: %q, %v", loaded.AdminListen, err)
		}
	}
	for _, address := range []string{"", ":8081", "0.0.0.0:8081", "[::]:8081", "192.168.1.10:8081", "example.com:8081", "localhost.example.com:8081", "127.0.0.1", "127.0.0.1:http", "127.0.0.1:-1", "127.0.0.1:65536", Default().Listen} {
		cfg := Default()
		cfg.AdminListen = address
		if err := cfg.Validate(); err == nil {
			t.Errorf("invalid admin_listen accepted: %q", address)
		}
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("listen: 127.0.0.1:8080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(configPath)
	if err != nil || loaded.AdminListen != "127.0.0.1:8081" || loaded.AdminURL() != "http://127.0.0.1:8081" {
		t.Fatalf("default management addresses: %+v, %v", loaded, err)
	}
}

func TestLegacyServerURLIsIgnoredAndRemovedOnSave(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("admin_listen: localhost:9123\nserver_url: http://obsolete.example.com:8080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil || cfg.AdminURL() != "http://127.0.0.1:9123" {
		t.Fatalf("obsolete server_url affected the admin endpoint: %q, %v", cfg.AdminURL(), err)
	}
	if err := Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil || bytes.Contains(data, []byte("server_url:")) {
		t.Fatalf("obsolete server_url was persisted: %s, %v", data, err)
	}
	if err := os.WriteFile(configPath, []byte("unknown_setting: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(configPath); err == nil {
		t.Fatal("legacy support disabled unknown-field validation")
	}
}
