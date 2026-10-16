package config

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const MiB = 1024 * 1024

type Resource struct {
	ID   string `yaml:"id" json:"id"`
	Path string `yaml:"path" json:"path"`
}

type Config struct {
	Listen           string     `yaml:"listen"`
	AdminListen      string     `yaml:"admin_listen"`
	ClientWSURL      string     `yaml:"client_ws_url"`
	WSPath           string     `yaml:"ws_path"`
	ChunkBytes       int64      `yaml:"chunk_bytes"`
	ChannelBytes     int64      `yaml:"channel_bytes"`
	ClientConcurrent int        `yaml:"client_concurrency"`
	Workers          int        `yaml:"workers"`
	PendingRequests  int        `yaml:"pending_requests"`
	AllowedOrigins   []string   `yaml:"allowed_origins"`
	Resources        []Resource `yaml:"resources"`
}

func Default() Config {
	return Config{
		Listen: "127.0.0.1:8080", AdminListen: "127.0.0.1:8081",
		WSPath:     "/ws",
		ChunkBytes: 4 * MiB, ChannelBytes: 32 * MiB,
		ClientConcurrent: 8, Workers: 8, PendingRequests: 64,
		AllowedOrigins: []string{}, Resources: []Resource{},
	}
}

func (c Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen 不能为空")
	}
	adminHost, adminPort, err := net.SplitHostPort(c.AdminListen)
	if err != nil || (adminHost != "localhost" && !net.ParseIP(adminHost).IsLoopback()) {
		return errors.New("admin_listen 必须使用本机回环地址，例如 127.0.0.1:8081、localhost:8081 或 [::1]:8081")
	}
	port, err := strconv.Atoi(adminPort)
	if err != nil || port < 0 || port > 65535 {
		return errors.New("admin_listen 的端口必须在 0～65535 内")
	}
	if c.Listen == c.AdminListen && port != 0 {
		return errors.New("listen 与 admin_listen 必须使用不同端口")
	}
	if c.ClientWSURL != "" {
		u, err := url.Parse(c.ClientWSURL)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
			return errors.New("client_ws_url 必须是 WS/WSS 地址，不能包含用户名、密码、查询参数或片段")
		}
	}
	wsURL, err := url.Parse(c.WSPath)
	if err != nil || !strings.HasPrefix(c.WSPath, "/") || wsURL.Path != c.WSPath || strings.Contains(c.WSPath, `\`) || path.Clean(c.WSPath) != strings.TrimSuffix(c.WSPath, "/") || c.WSPath == "/" || c.WSPath == "/healthz" || c.WSPath == "/api" || strings.HasPrefix(c.WSPath, "/api/") {
		return errors.New("ws_path 必须是以 / 开头的规范路径，不能包含查询参数、片段或与内置接口冲突")
	}
	if c.ChunkBytes < 1 || c.ChunkBytes > 64*MiB || c.ChannelBytes < c.ChunkBytes || c.ChannelBytes > 1024*MiB {
		return errors.New("chunk_bytes 须在 1～64 MiB 内，channel_bytes 须不小于块大小且不超过 1 GiB")
	}
	if c.Workers < 1 || c.Workers > 64 || c.ClientConcurrent < 1 || c.ClientConcurrent > 64 || c.PendingRequests < 1 || c.PendingRequests > 4096 {
		return errors.New("workers 和 client_concurrency 须在 1～64 内，pending_requests 须在 1～4096 内")
	}
	for _, origin := range c.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return fmt.Errorf("allowed_origins 中的 %q 须为 HTTP/HTTPS origin（不带路径）", origin)
		}
	}
	seen := map[string]bool{}
	for _, r := range c.Resources {
		if !filepath.IsAbs(r.Path) || r.ID != ID(r.Path) || seen[r.ID] {
			return fmt.Errorf("资源 %q 的路径、ID 或重复配置无效", r.Path)
		}
		seen[r.ID] = true
	}
	return nil
}

func (c Config) AdminAddress() string {
	host, port, _ := net.SplitHostPort(c.AdminListen)
	if host == "localhost" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (c Config) AdminURL() string { return "http://" + c.AdminAddress() }

func ID(absolutePath string) string {
	sum := md5.Sum([]byte(filepath.Clean(absolutePath)))
	return hex.EncodeToString(sum[:])
}

func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	// Accept but ignore the obsolete endpoint. Saving removes it from the YAML.
	document := struct {
		Config          `yaml:",inline"`
		LegacyServerURL any `yaml:"server_url"`
	}{Config: c}
	err = d.Decode(&document)
	c = document.Config
	if err != nil {
		return c, fmt.Errorf("解析配置: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, errors.New("配置只允许一个 YAML 文档")
	}
	return c, c.Validate()
}

// Save writes and syncs a temporary file before replacing the configuration.
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return WriteAtomic(path, b, 0600)
}

func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".wsdownloader-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

// A directory lock works across CLI processes on all three target systems.
func Lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	lock := path + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Mkdir(lock, 0700)
		if err == nil {
			return func() { _ = os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("配置被其他进程锁定: %s（若进程已退出，可移除此空锁目录）", lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
