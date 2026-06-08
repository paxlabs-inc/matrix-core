package objstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// MemStore is an in-memory blob store for dev and tests.
type MemStore struct {
	mu      sync.RWMutex
	objects map[string][]byte
	bucket  string
}

// NewMem returns an empty in-memory store.
func NewMem(bucket string) *MemStore {
	if bucket == "" {
		bucket = "deus-dev"
	}
	return &MemStore{objects: make(map[string][]byte), bucket: bucket}
}

// Put stores bytes at key.
func (m *MemStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	_ = ctx
	_ = size
	_ = contentType
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.objects[key] = raw
	m.mu.Unlock()
	return nil
}

// Get opens an object for reading.
func (m *MemStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	_ = ctx
	m.mu.RLock()
	raw, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("objstore: key not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

// URL returns a pseudo reference for the object key.
func (m *MemStore) URL(key string) string {
	return fmt.Sprintf("mem://%s/%s", m.bucket, key)
}
