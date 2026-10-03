// Package resolve resolves localized content with a deterministic fallback
// chain. Missing translations are distinguished from explicitly empty values.
package resolve

import (
	"fmt"

	"github.com/Diudiuseven/cms-content-sync-go/internal/config"
	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/store"
)

// Resolver resolves content across locales.
type Resolver struct {
	versions *store.VersionStore
	config   *config.Config
}

// NewResolver creates a resolver.
func NewResolver(versions *store.VersionStore, cfg *config.Config) *Resolver {
	return &Resolver{versions: versions, config: cfg}
}

// Resolution describes how a field value was resolved.
type Resolution struct {
	Value     interface{}
	Found     bool   // a value was found (possibly empty)
	FellBack  bool   // the value came from a fallback locale
	Locale    string // the locale the value came from
	Explicit  bool   // the value was explicitly set (Set=true), even if empty
}

// ResolveField resolves a single field for content id in the requested locale.
// It walks the fallback chain [locale, language, default] and returns the
// first value found. An explicitly empty value (Set=true, Value="") is
// returned as found with Explicit=true and does NOT trigger further fallback.
func (r *Resolver) ResolveField(id, locale, field string) (*Resolution, error) {
	version, err := r.versions.Current()
	if err != nil {
		return nil, err
	}
	chain := r.config.FallbackChain(locale)
	for _, loc := range chain {
		key := model.ContentKey{ID: id, Locale: loc}
		c, ok := version.Get(key)
		if !ok {
			continue
		}
		fv, ok := c.Fields[field]
		if !ok || !fv.Set {
			// Field absent in this locale: try the next fallback.
			continue
		}
		return &Resolution{
			Value:    fv.Value,
			Found:    true,
			FellBack: loc != locale,
			Locale:   loc,
			Explicit: true,
		}, nil
	}
	return &Resolution{Found: false}, nil
}

// ResolveContent resolves all fields for content id in the requested locale,
// applying fallback per field. It returns a map of field name to Resolution.
func (r *Resolver) ResolveContent(id, locale string) (map[string]*Resolution, error) {
	version, err := r.versions.Current()
	if err != nil {
		return nil, err
	}
	// Collect all field names across the fallback chain.
	fieldNames := make(map[string]struct{})
	chain := r.config.FallbackChain(locale)
	for _, loc := range chain {
		key := model.ContentKey{ID: id, Locale: loc}
		if c, ok := version.Get(key); ok {
			for name := range c.Fields {
				fieldNames[name] = struct{}{}
			}
		}
	}
	out := make(map[string]*Resolution, len(fieldNames))
	for name := range fieldNames {
		res, err := r.ResolveField(id, locale, name)
		if err != nil {
			return nil, fmt.Errorf("resolve field %q: %w", name, err)
		}
		out[name] = res
	}
	return out, nil
}
