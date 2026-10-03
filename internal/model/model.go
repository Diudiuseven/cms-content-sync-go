// Package model defines the core content types shared across the sync engine.
package model

import (
	"errors"
	"time"
)

// FieldType enumerates the supported field data types.
type FieldType string

const (
	TypeString    FieldType = "string"
	TypeText      FieldType = "text"
	TypeInt       FieldType = "int"
	TypeNumber    FieldType = "number"
	TypeBool      FieldType = "bool"
	TypeDate      FieldType = "date"
	TypeEnum      FieldType = "enum"
	TypeReference FieldType = "reference"
)

// FieldSchema describes one field of a content model.
type FieldSchema struct {
	Name      string    `json:"name"`
	Type      FieldType `json:"type"`
	Required  bool      `json:"required"`
	Localized bool      `json:"localized"`
	Enum      []string  `json:"enum,omitempty"`
}

// Schema is a versioned content model definition.
type Schema struct {
	ID      string                  `json:"id"`
	Version int                     `json:"version"`
	Fields  map[string]FieldSchema  `json:"fields"`
}

// FieldValue represents a single field value. The Set flag distinguishes an
// explicitly provided value (which may be the empty string) from a missing
// value that should trigger locale fallback.
type FieldValue struct {
	Set   bool        `json:"set"`
	Value interface{} `json:"value,omitempty"`
}

// ContentKey identifies a piece of content within a locale.
type ContentKey struct {
	ID     string `json:"id"`
	Locale string `json:"locale"`
}

// Content is a single content item (draft or published).
type Content struct {
	ID            string             `json:"id"`
	Locale        string             `json:"locale"`
	SchemaID      string             `json:"schema_id"`
	SchemaVersion int                `json:"schema_version"`
	Fields        map[string]FieldValue `json:"fields"`
	Deleted       bool               `json:"deleted"`
	LastSeq       int64              `json:"last_seq"`
}

// Key returns the ContentKey for this content.
func (c *Content) Key() ContentKey {
	return ContentKey{ID: c.ID, Locale: c.Locale}
}

// EventType enumerates change event kinds.
type EventType int

const (
	EventUpsert EventType = iota
	EventDelete
)

// Event is an upstream change event. Seq is a monotonically increasing,
// contiguous sequence number assigned by the upstream source.
type Event struct {
	Seq           int64              `json:"seq"`
	Type          EventType          `json:"type"`
	ID            string             `json:"id"`
	Locale        string             `json:"locale"`
	SchemaID      string             `json:"schema_id"`
	SchemaVersion int                `json:"schema_version"`
	Fields        map[string]FieldValue `json:"fields,omitempty"`
	Timestamp     time.Time          `json:"timestamp"`
}

// Key returns the ContentKey this event affects.
func (e *Event) Key() ContentKey {
	return ContentKey{ID: e.ID, Locale: e.Locale}
}

// Snapshot is a full point-in-time dump of upstream content.
type Snapshot struct {
	Seq       int64                    `json:"seq"`
	Contents  map[ContentKey]*Content  `json:"contents"`
	TakenAt   time.Time                `json:"taken_at"`
}

// Common errors.
var (
	ErrNotFound           = errors.New("not found")
	ErrSchemaIncompatible = errors.New("schema incompatible")
	ErrValidation         = errors.New("validation failed")
	ErrInvalidCursor      = errors.New("invalid cursor")
)
