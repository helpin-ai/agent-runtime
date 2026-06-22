package skills

import (
	"fmt"
	"strings"
)

type PackageStoreRegistry struct {
	stores map[string]PackageStore
}

func NewPackageStoreRegistry() *PackageStoreRegistry {
	return &PackageStoreRegistry{stores: map[string]PackageStore{}}
}

func (r *PackageStoreRegistry) Register(appID string, store PackageStore) error {
	if r == nil {
		return fmt.Errorf("package store registry is not configured")
	}
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return fmt.Errorf("app_id is required")
	}
	if store == nil {
		return fmt.Errorf("package store is required")
	}
	r.stores[appID] = store
	return nil
}

func (r *PackageStoreRegistry) Store(appID string) (PackageStore, bool) {
	if r == nil {
		return nil, false
	}
	store, ok := r.stores[strings.TrimSpace(appID)]
	return store, ok
}
