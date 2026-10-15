package config

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
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
	ServerURL        string     `yaml:"server_url"`
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
		Listen: "127.0.0.1:8080", ServerURL: "http://127.0.0.1:8080",
		ChunkBytes: 4 * MiB, ChannelBytes: 32 * MiB,
		ClientConcurrent: 8, Workers: 8, PendingRequests: 64,
		AllowedOrigins: []string{}, Resources: []Resource{},
	}
}

func (c Config) Validate() error {
	u, err := url.Parse(c.ServerURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("server_url 必须是无路径的 HTTP/HTTPS 地址")
	}
	if c.Listen == "" {
		return errors.New("listen 不能为空")
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
	if err = d.Decode(&c); err != nil {
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
