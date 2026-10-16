package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"wsdownloader/internal/config"
)

type Entry struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	source string
}

type Warning struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type Manifest struct {
	ResourceID         string    `json:"resource_id"`
	Name               string    `json:"name"`
	ChunkBytes         int64     `json:"chunk_bytes"`
	ClientConcurrency  int       `json:"client_concurrency"`
	MaxPendingRequests int       `json:"max_pending_requests"`
	Entries            []Entry   `json:"entries"`
	Warnings           []Warning `json:"warnings"`
	files              map[string]Entry
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Only ancestors, rather than a global visited set, are checked: two aliases
// of the same directory are both legitimate download entries.
func buildManifest(ctx context.Context, resource config.Resource, cfg config.Config, hashes *hashCache) (*Manifest, error) {
	name := filepath.Base(filepath.Clean(resource.Path))
	if name == "." || strings.ContainsAny(name, `\/:`) {
		name = "root"
	}
	m := &Manifest{ResourceID: resource.ID, Name: name, ChunkBytes: cfg.ChunkBytes,
		ClientConcurrency: min(cfg.ClientConcurrent, cfg.PendingRequests), MaxPendingRequests: cfg.PendingRequests,
		Entries: []Entry{}, Warnings: []Warning{}, files: map[string]Entry{}}
	var walk func(string, string, []os.FileInfo, int) error
	walk = func(source, relative string, ancestors []os.FileInfo, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 256 {
			return fmt.Errorf("目录层级超过 256，可能存在递归链接")
		}
		real, err := filepath.EvalSymlinks(source)
		if err != nil {
			return err
		}
		info, err := os.Stat(real)
		if err != nil {
			return err
		}
		if hashes.isCacheFile(info) {
			return nil
		}
		entry := Entry{ID: relative, Path: relative, Size: info.Size(), source: real}
		if info.IsDir() {
			for _, ancestor := range ancestors {
				if os.SameFile(ancestor, info) {
					return fmt.Errorf("跳过循环目录链接")
				}
			}
			children, err := os.ReadDir(real)
			if err != nil {
				return err
			}
			entry.Kind, entry.Size = "directory", 0
			m.Entries = append(m.Entries, entry)
			ancestors = append(append([]os.FileInfo{}, ancestors...), info)
			for _, child := range children {
				rel := path.Join(relative, child.Name())
				if err := walk(filepath.Join(real, child.Name()), rel, ancestors, depth+1); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					m.Warnings = append(m.Warnings, Warning{Path: rel, Message: err.Error()})
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("不支持普通文件、目录以外的条目")
		}
		sum, err := hashes.sum(ctx, real, info)
		if err != nil {
			return err
		}
		entry.Kind, entry.SHA256 = "file", sum
		m.Entries = append(m.Entries, entry)
		m.files[entry.ID] = entry
		return nil
	}
	if err := walk(resource.Path, name, nil, 0); err != nil {
		return nil, err
	}
	return m, nil
}
