package schema_test

import (
	"errors"
	"testing"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/schema"
)

func baseSchema(version int) *model.Schema {
	return &model.Schema{
		ID:      "article",
		Version: version,
		Fields: map[string]model.FieldSchema{
			"title":    {Name: "title", Type: model.TypeString, Required: true},
			"body":     {Name: "body", Type: model.TypeText, Required: false},
			"category": {Name: "category", Type: model.TypeEnum, Required: false, Enum: []string{"a", "b"}},
		},
	}
}

func TestRegisterAndGet(t *testing.T) {
	r := schema.NewRegistry()
	if err := r.Register(baseSchema(1)); err != nil {
		t.Fatalf("register: %v", err)
	}
	s, err := r.Get("article", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.ID != "article" || s.Version != 1 {
		t.Fatalf("got wrong schema: %+v", s)
	}
	latest, err := r.Latest("article")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest != 1 {
		t.Fatalf("latest = %d, want 1", latest)
	}
}

func TestAddFieldCompatible(t *testing.T) {
	r := schema.NewRegistry()
	if err := r.Register(baseSchema(1)); err != nil {
		t.Fatalf("register v1: %v", err)
	}
	v2 := baseSchema(2)
	v2.Fields["subtitle"] = model.FieldSchema{Name: "subtitle", Type: model.TypeString, Required: false}
	if err := r.Register(v2); err != nil {
		t.Fatalf("add field should be compatible: %v", err)
	}
}

func TestRemoveRequiredFieldRejected(t *testing.T) {
	r := schema.NewRegistry()
	_ = r.Register(baseSchema(1))
	v2 := &model.Schema{
		ID: "article", Version: 2,
		Fields: map[string]model.FieldSchema{
			"body:": {Name: "body", Type: model.TypeText, Required: false},
		},
	}
	// typo guard: build correctly
	v2.Fields = map[string]model.FieldSchema{
		"body": {Name: "body", Type: model.TypeText, Required: false},
	}
	err := r.Register(v2)
	assertReason(t, err, schema.ReasonRequiredFieldRemoved, "title")
}

func TestRenameRequiredFieldRejected(t *testing.T) {
	r := schema.NewRegistry()
	_ = r.Register(baseSchema(1))
	v2 := &model.Schema{
		ID: "article", Version: 2,
		Fields: map[string]model.FieldSchema{
			"headline": {Name: "headline", Type: model.TypeString, Required: true},
			"body":     {Name: "body", Type: model.TypeText, Required: false},
		},
	}
	err := r.Register(v2)
	assertReason(t, err, schema.ReasonRequiredFieldRemoved, "title")
}

func TestChangeTypeRejected(t *testing.T) {
	r := schema.NewRegistry()
	_ = r.Register(baseSchema(1))
	v2 := baseSchema(2)
	v2.Fields["title"] = model.FieldSchema{Name: "title", Type: model.TypeInt, Required: true}
	err := r.Register(v2)
	assertReason(t, err, schema.ReasonTypeChanged, "title")
}

func TestRemoveEnumValueRejected(t *testing.T) {
	r := schema.NewRegistry()
	_ = r.Register(baseSchema(1))
	v2 := baseSchema(2)
	v2.Fields["category"] = model.FieldSchema{Name: "category", Type: model.TypeEnum, Required: false, Enum: []string{"a"}}
	err := r.Register(v2)
	assertReason(t, err, schema.ReasonEnumValueRemoved, "category")
}

func TestRemoveOptionalFieldAllowed(t *testing.T) {
	r := schema.NewRegistry()
	_ = r.Register(baseSchema(1))
	v2 := &model.Schema{
		ID: "article", Version: 2,
		Fields: map[string]model.FieldSchema{
			"title": {Name: "title", Type: model.TypeString, Required: true},
		},
	}
	if err := r.Register(v2); err != nil {
		t.Fatalf("removing optional field should be allowed: %v", err)
	}
}

func TestStringTextCompatible(t *testing.T) {
	r := schema.NewRegistry()
	_ = r.Register(baseSchema(1))
	v2 := baseSchema(2)
	v2.Fields["title"] = model.FieldSchema{Name: "title", Type: model.TypeText, Required: true}
	if err := r.Register(v2); err != nil {
		t.Fatalf("string->text should be compatible: %v", err)
	}
}

func assertReason(t *testing.T, err error, want schema.IncompatibilityReason, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected rejection, got nil")
	}
	var incompat *schema.IncompatibilityError
	if !errors.As(err, &incompat) {
		t.Fatalf("expected IncompatibilityError, got %T: %v", err, err)
	}
	if incompat.Reason != want {
		t.Fatalf("reason = %s, want %s", incompat.Reason, want)
	}
	if incompat.Field != wantField {
		t.Fatalf("field = %s, want %s", incompat.Field, wantField)
	}
}
