// Package schema manages content model versions and compatibility checks.
package schema

import (
	"fmt"
	"sync"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
)

// IncompatibilityReason is a distinguishable cause for rejecting a schema.
type IncompatibilityReason string

const (
	ReasonRequiredFieldRemoved IncompatibilityReason = "required_field_removed"
	ReasonRequiredFieldRenamed IncompatibilityReason = "required_field_renamed"
	ReasonTypeChanged          IncompatibilityReason = "type_changed"
	ReasonEnumValueRemoved     IncompatibilityReason = "enum_value_removed"
)

// IncompatibilityError carries a machine-distinguishable reason.
type IncompatibilityError struct {
	Reason    IncompatibilityReason
	Field     string
	Detail    string
	OldSchema string
	NewSchema string
}

func (e *IncompatibilityError) Error() string {
	return fmt.Sprintf("schema incompatible: reason=%s field=%s detail=%s", e.Reason, e.Field, e.Detail)
}

// Registry holds all known schema versions.
type Registry struct {
	mu      sync.RWMutex
	schemas map[string]map[int]*model.Schema // schemaID -> version -> schema
	latest  map[string]int                    // schemaID -> latest version
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		schemas: make(map[string]map[int]*model.Schema),
		latest:  make(map[string]int),
	}
}

// Register adds a new schema version. It rejects incompatible changes to
// required fields with a distinguishable reason.
func (r *Registry) Register(s *model.Schema) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if s.Version < 1 {
		return fmt.Errorf("%w: schema version must be >= 1", model.ErrSchemaIncompatible)
	}

	versions, ok := r.schemas[s.ID]
	if !ok {
		versions = make(map[int]*model.Schema)
		r.schemas[s.ID] = versions
	}

	if _, exists := versions[s.Version]; exists {
		return fmt.Errorf("%w: schema %s version %d already registered", model.ErrSchemaIncompatible, s.ID, s.Version)
	}

	if prevVersion, ok := r.latest[s.ID]; ok {
		prev := versions[prevVersion]
		if err := checkCompatible(prev, s); err != nil {
			return err
		}
	}

	versions[s.Version] = s
	r.latest[s.ID] = s.Version
	return nil
}

// Get retrieves a specific schema version.
func (r *Registry) Get(id string, version int) (*model.Schema, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	versions, ok := r.schemas[id]
	if !ok {
		return nil, fmt.Errorf("%w: schema %s", model.ErrNotFound, id)
	}
	s, ok := versions[version]
	if !ok {
		return nil, fmt.Errorf("%w: schema %s version %d", model.ErrNotFound, id, version)
	}
	return s, nil
}

// Latest returns the latest version number for a schema id.
func (r *Registry) Latest(id string) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.latest[id]
	if !ok {
		return 0, fmt.Errorf("%w: schema %s", model.ErrNotFound, id)
	}
	return v, nil
}

// checkCompatible verifies that newSchema is a compatible evolution of prev.
// Required fields must not be removed or renamed; field types must remain
// compatible. New fields may be added freely (and are preserved as-is).
func checkCompatible(prev, next *model.Schema) error {
	for name, prevField := range prev.Fields {
		nextField, ok := next.Fields[name]
		if !ok {
			if prevField.Required {
				return &IncompatibilityError{
					Reason:    ReasonRequiredFieldRemoved,
					Field:     name,
					Detail:    fmt.Sprintf("required field %q removed", name),
					OldSchema: fmt.Sprintf("%s@%d", prev.ID, prev.Version),
					NewSchema: fmt.Sprintf("%s@%d", next.ID, next.Version),
				}
			}
			// Optional field removed: allowed; old data is preserved.
			continue
		}
		if !typesCompatible(prevField.Type, nextField.Type) {
			return &IncompatibilityError{
				Reason:    ReasonTypeChanged,
				Field:     name,
				Detail:    fmt.Sprintf("field %q type changed from %s to %s", name, prevField.Type, nextField.Type),
				OldSchema: fmt.Sprintf("%s@%d", prev.ID, prev.Version),
				NewSchema: fmt.Sprintf("%s@%d", next.ID, next.Version),
			}
		}
		if prevField.Type == model.TypeEnum {
			for _, v := range prevField.Enum {
				if !contains(nextField.Enum, v) {
					return &IncompatibilityError{
						Reason:    ReasonEnumValueRemoved,
						Field:     name,
						Detail:    fmt.Sprintf("enum value %q removed from field %q", v, name),
						OldSchema: fmt.Sprintf("%s@%d", prev.ID, prev.Version),
						NewSchema: fmt.Sprintf("%s@%d", next.ID, next.Version),
					}
				}
			}
		}
	}
	return nil
}

// typesCompatible reports whether a field type can evolve into another.
// string <-> text are mutually compatible; all other types must match exactly.
func typesCompatible(prev, next model.FieldType) bool {
	if prev == next {
		return true
	}
	if (prev == model.TypeString && next == model.TypeText) ||
		(prev == model.TypeText && next == model.TypeString) {
		return true
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
