package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var hashBucket = []byte("file-sha256-v1")

type hashRecord struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime_ns"`
	SHA256  string `json:"sha256"`
}

type hashCall struct {
	done chan struct{}
	err  error
}

type hashCache struct {
	db       *bolt.DB
	info     os.FileInfo
	mu       sync.Mutex
	inflight map[string]*hashCall
	readHash func(context.Context, string, os.FileInfo) (string, error)
}

func hashCachePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "sha256.kv")
}

func openHashCache(path string) (*hashCache, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := bolt.Open(absolute, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(hashBucket)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &hashCache{db: db, info: info, inflight: make(map[string]*hashCall), readHash: readFileHash}, nil
}

func (c *hashCache) close() error { return c.db.Close() }

func (c *hashCache) isCacheFile(info os.FileInfo) bool {
	return c != nil && os.SameFile(c.info, info)
}

func (c *hashCache) sum(ctx context.Context, path string, info os.FileInfo) (string, error) {
	if c == nil {
		return readFileHash(ctx, path, info)
	}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		c.mu.Lock()
		if call := c.inflight[path]; call != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-call.done:
				// A cancelled first requester must not cancel other downloads.
				if call.err != nil && !errors.Is(call.err, context.Canceled) && !errors.Is(call.err, context.DeadlineExceeded) {
					return "", call.err
				}
				continue
			}
		}
		call := &hashCall{done: make(chan struct{})}
		c.inflight[path] = call
		c.mu.Unlock()
		sum, err := c.loadOrHash(ctx, path, info)
		c.mu.Lock()
		call.err = err
		delete(c.inflight, path)
		close(call.done)
		c.mu.Unlock()
		return sum, err
	}
}

func (c *hashCache) loadOrHash(ctx context.Context, path string, info os.FileInfo) (string, error) {
	var record hashRecord
	err := c.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(hashBucket).Get([]byte(path))
		if data != nil {
			// Invalid entries are disposable; recalculate rather than trust them.
			if json.Unmarshal(data, &record) != nil {
				record = hashRecord{}
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("读取 SHA-256 缓存: %w", err)
	}
	if record.Size == info.Size() && record.ModTime == info.ModTime().UnixNano() && len(record.SHA256) == 64 {
		if _, err := hex.DecodeString(record.SHA256); err == nil {
			return record.SHA256, nil
		}
	}
	sum, err := c.readHash(ctx, path, info)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	after, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !sameFileVersion(info, after) {
		return "", errors.New("源文件在计算 SHA-256 时发生变化，请重新获取文件列表")
	}
	value, err := json.Marshal(hashRecord{Size: info.Size(), ModTime: info.ModTime().UnixNano(), SHA256: sum})
	if err != nil {
		return "", err
	}
	if err := c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(hashBucket).Put([]byte(path), value)
	}); err != nil {
		return "", fmt.Errorf("保存 SHA-256 缓存: %w", err)
	}
	return sum, nil
}

func sameFileVersion(first, second os.FileInfo) bool {
	return os.SameFile(first, second) && first.Size() == second.Size() && first.ModTime().Equal(second.ModTime())
}

func readFileHash(ctx context.Context, path string, expected os.FileInfo) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !sameFileVersion(expected, before) {
		return "", errors.New("源文件在计算 SHA-256 前发生变化，请重新获取文件列表")
	}
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx: ctx, r: f})
	if err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if n != before.Size() || !sameFileVersion(before, after) {
		return "", errors.New("源文件在计算 SHA-256 时发生变化，请重新获取文件列表")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
