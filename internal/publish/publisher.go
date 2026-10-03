// Package publish handles atomic versioned publishing. Readers always observe
// a single committed version; historical versions remain queryable.
package publish

import (
	"fmt"
	"sort"
	"sync"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/schema"
	"github.com/Diudiuseven/cms-content-sync-go/internal/store"
)

// Publisher validates drafts and atomically publishes immutable versions.
type Publisher struct {
	mu       sync.Mutex
	versions *store.VersionStore
	registry *schema.Registry
}

// NewPublisher creates a publisher.
func NewPublisher(versions *store.VersionStore, registry *schema.Registry) *Publisher {
	return &Publisher{versions: versions, registry: registry}
}

// Validate checks that all published content conforms to its registered
// schema. Unknown fields are allowed (they are preserved through round-trip);
// known fields must have compatible types and required fields must be present.
func (p *Publisher) Validate(contents map[model.ContentKey]*model.Content) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	keys := make([]model.ContentKey, 0, len(contents))
	for k := range contents {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ID != keys[j].ID {
			return keys[i].ID < keys[j].ID
		}
		return keys[i].Locale < keys[j].Locale
	})

	for _, k := range keys {
		c := contents[k]
		if c.Deleted {
			continue
		}
		if err := p.validateContent(c); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) validateContent(c *model.Content) error {
	if c.SchemaID == "" {
		// No schema registered for this content; preserve as-is.
		return nil
	}
	sch, err := p.registry.Get(c.SchemaID, c.SchemaVersion)
	if err != nil {
		// Schema version unknown to registry; preserve as-is.
		return nil
	}
	for name, fs := range sch.Fields {
		if fs.Required {
			fv, ok := c.Fields[name]
			if !ok || !fv.Set {
				return fmt.Errorf("%w: content %s locale %s missing required field %q",
					model.ErrValidation, c.ID, c.Locale, name)
			}
		}
		if fv, ok := c.Fields[name]; ok && fv.Set {
			if err := checkType(fs.Type, fv.Value); err != nil {
				return fmt.Errorf("%w: content %s locale %s field %q: %v",
					model.ErrValidation, c.ID, c.Locale, name, err)
			}
		}
	}
	return nil
}

func checkType(t model.FieldType, v interface{}) error {
	switch t {
	case model.TypeString, model.TypeText:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string, got %T", v)
		}
	case model.TypeInt:
		switch v.(type) {
		case int, int64, float64:
			return nil
		}
		return fmt.Errorf("expected int, got %T", v)
	case model.TypeNumber:
		switch v.(type) {
		case int, int64, float64:
			return nil
		}
		return fmt.Errorf("expected number, got %T", v)
	case model.TypeBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected bool, got %T", v)
		}
	case model.TypeDate:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected date string, got %T", v)
		}
	case model.TypeEnum:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected enum string, got %T", v)
		}
	case model.TypeReference:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected reference string, got %T", v)
		}
	}
	return nil
}

// Publish validates and atomically publishes a new version.
func (p *Publisher) Publish(contents map[model.ContentKey]*model.Content, cursor int64) (*store.Version, error) {
	if err := p.Validate(contents); err != nil {
		return nil, err
	}
	v := p.versions.Publish(contents, cursor)
	return v, nil
}

// Current returns the currently published version.
func (p *Publisher) Current() (*store.Version, error) {
	return p.versions.Current()
}

// Get returns a specific historical version.
func (p *Publisher) Get(number int64) (*store.Version, error) {
	return p.versions.Get(number)
}

// List returns all published version numbers.
func (p *Publisher) List() []int64 {
	return p.versions.List()
}

// LatestNumber returns the latest version number.
func (p *Publisher) LatestNumber() int64 {
	return p.versions.LatestNumber()
}
