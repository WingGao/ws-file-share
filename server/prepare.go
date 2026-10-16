package server

import (
	"context"
	"fmt"

	"wsdownloader/internal/config"
)

type Preparation struct {
	Files    int `json:"files"`
	Warnings int `json:"warnings"`
}

// PrepareResource warms the persistent cache while the service is offline.
func PrepareResource(ctx context.Context, configPath string, cfg config.Config, resource config.Resource) (Preparation, error) {
	hashes, err := openHashCache(hashCachePath(configPath))
	if err != nil {
		return Preparation{}, fmt.Errorf("打开 SHA-256 缓存: %w", err)
	}
	defer hashes.close()
	return prepareResource(ctx, resource, cfg, hashes)
}

func prepareResource(ctx context.Context, resource config.Resource, cfg config.Config, hashes *hashCache) (Preparation, error) {
	m, err := buildManifest(ctx, resource, cfg, hashes)
	if err != nil {
		return Preparation{}, err
	}
	return Preparation{Files: len(m.files), Warnings: len(m.Warnings)}, nil
}
