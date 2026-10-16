package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"wsdownloader/internal/config"
)

func TestHashCachePersistsAndInvalidates(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "data.bin")
	cachePath := filepath.Join(dir, "sha256.kv")
	if err := os.WriteFile(file, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	cache, err := openHashCache(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.close() })
	reads := 0
	reader := func(ctx context.Context, path string, info os.FileInfo) (string, error) {
		reads++
		return readFileHash(ctx, path, info)
	}
	cache.readHash = reader
	check := func(content string, wantReads int) os.FileInfo {
		t.Helper()
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := cache.sum(context.Background(), file, info)
		want := sha256.Sum256([]byte(content))
		if err != nil || got != hex.EncodeToString(want[:]) || reads != wantReads {
			t.Fatalf("hash=%q reads=%d wantReads=%d err=%v", got, reads, wantReads, err)
		}
		return info
	}
	initial := check("one", 1)
	check("one", 1)
	if err := cache.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openHashCache(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	cache = reopened
	cache.readHash = reader
	check("one", 1)
	// Size alone must invalidate the entry even when the mtime is preserved.
	if err := os.WriteFile(file, []byte("longer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, initial.ModTime(), initial.ModTime()); err != nil {
		t.Fatal(err)
	}
	check("longer", 2)
	// An equal-size edit must invalidate the entry when the mtime changes.
	if err := os.WriteFile(file, []byte("LONGER"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := initial.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(file, changed, changed); err != nil {
		t.Fatal(err)
	}
	check("LONGER", 3)
	if err := cache.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(hashBucket).Put([]byte(file), []byte(`{"size":"invalid"}`))
	}); err != nil {
		t.Fatal(err)
	}
	check("LONGER", 4)
}

func TestHashCacheCoalescesRequestsAndAllowsWaiterCancellation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "data")
	if err := os.WriteFile(file, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := openHashCache(filepath.Join(dir, "hashes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.close()
	started, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	cache.readHash = func(ctx context.Context, path string, info os.FileInfo) (string, error) {
		if reads.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return readFileHash(ctx, path, info)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := cache.sum(context.Background(), file, info); err != nil {
				t.Error(err)
			}
		})
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() {
		_, err := cache.sum(ctx, file, info)
		cancelled <- err
	}()
	cancel()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("cancelled waiter remained blocked")
	}
	close(release)
	wg.Wait()
	if reads.Load() != 1 {
		t.Fatalf("concurrent requests read the file %d times", reads.Load())
	}
}

func TestCancelledHashDoesNotPoisonOtherRequests(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "data")
	if err := os.WriteFile(file, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := openHashCache(filepath.Join(dir, "hashes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.close()
	started := make(chan struct{})
	var reads atomic.Int32
	cache.readHash = func(ctx context.Context, path string, info os.FileInfo) (string, error) {
		if reads.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return readFileHash(ctx, path, info)
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := cache.sum(ctx, file, info); first <- err }()
	<-started
	second := make(chan error, 1)
	go func() { _, err := cache.sum(context.Background(), file, info); second <- err }()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first request: %v", err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second request was not retried")
	}
	if reads.Load() != 2 {
		t.Fatalf("expected one retry, got %d reads", reads.Load())
	}
}

func TestHashCacheRejectsChangedFilesAndManifestSkipsCache(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "data")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := openHashCache(filepath.Join(dir, "sha256.kv"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.close()
	cache.readHash = func(ctx context.Context, path string, expected os.FileInfo) (string, error) {
		sum, err := readFileHash(ctx, path, expected)
		if err != nil {
			return "", err
		}
		return sum, os.WriteFile(path, []byte("changed content"), 0600)
	}
	if _, err := cache.sum(context.Background(), file, info); err == nil {
		t.Fatal("cached a file modified during hashing")
	}
	if err := cache.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(hashBucket).Get([]byte(file)) != nil {
			t.Error("unstable hash was persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	cache.readHash = func(ctx context.Context, path string, info os.FileInfo) (string, error) {
		reads++
		return readFileHash(ctx, path, info)
	}
	for range 2 {
		m, err := buildManifest(context.Background(), config.Resource{ID: config.ID(dir), Path: dir}, config.Default(), cache)
		if err != nil || len(m.files) != 1 || len(m.Warnings) != 0 {
			t.Fatalf("cache file entered manifest: %+v, %v", m, err)
		}
	}
	if reads != 1 {
		t.Fatalf("repeated manifests read data %d times", reads)
	}
}
