package gsync

import (
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// ProxyStorage implements storer.Storer by proxying object lookups
// to a central cache and falling back to a local filesystem storage.
type ProxyStorage struct {
	*filesystem.Storage
	cache storer.EncodedObjectStorer
}

// Ensure ProxyStorage implements storer.Storer
var _ storer.Storer = (*ProxyStorage)(nil)

// NewProxyStorage creates a new storage proxy.
func NewProxyStorage(cache storer.EncodedObjectStorer, local *filesystem.Storage) *ProxyStorage {
	return &ProxyStorage{
		Storage: local,
		cache:   cache,
	}
}

// EncodedObjectStorer implementation overrides

func (p *ProxyStorage) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	// Try local first
	obj, err := p.Storage.EncodedObject(t, h)
	if err == nil {
		return obj, nil
	}
	// Fallback to cache
	return p.cache.EncodedObject(t, h)
}

func (p *ProxyStorage) HasEncodedObject(h plumbing.Hash) error {
	if err := p.Storage.HasEncodedObject(h); err == nil {
		return nil
	}
	return p.cache.HasEncodedObject(h)
}

func (p *ProxyStorage) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	size, err := p.Storage.EncodedObjectSize(h)
	if err == nil {
		return size, nil
	}
	return p.cache.EncodedObjectSize(h)
}
