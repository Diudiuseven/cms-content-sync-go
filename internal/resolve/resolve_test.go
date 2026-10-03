package resolve_test

import (
	"testing"

	"github.com/Diudiuseven/cms-content-sync-go/internal/config"
	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/publish"
	"github.com/Diudiuseven/cms-content-sync-go/internal/resolve"
	"github.com/Diudiuseven/cms-content-sync-go/internal/schema"
	"github.com/Diudiuseven/cms-content-sync-go/internal/store"
)

func setupResolver(t *testing.T) (*resolve.Resolver, *store.VersionStore) {
	t.Helper()
	registry := schema.NewRegistry()
	_ = registry.Register(&model.Schema{
		ID: "article", Version: 1,
		Fields: map[string]model.FieldSchema{
			"title":    {Name: "title", Type: model.TypeString, Required: true, Localized: true},
			"subtitle": {Name: "subtitle", Type: model.TypeString, Required: false, Localized: true},
		},
	})
	versions := store.NewVersionStore()
	publisher := publish.NewPublisher(versions, registry)
	cfg := config.Default()
	resolver := resolve.NewResolver(versions, cfg)

	contents := map[model.ContentKey]*model.Content{
		{ID: "a1", Locale: "en"}: {
			ID: "a1", Locale: "en", SchemaID: "article", SchemaVersion: 1,
			Fields: map[string]model.FieldValue{
				"title":    {Set: true, Value: "Hello"},
				"subtitle": {Set: true, Value: ""}, // explicit empty
			},
		},
		{ID: "a1", Locale: "zh"}: {
			ID: "a1", Locale: "zh", SchemaID: "article", SchemaVersion: 1,
			Fields: map[string]model.FieldValue{
				"title": {Set: true, Value: "你好"},
				// subtitle missing -> fallback
			},
		},
	}
	if _, err := publisher.Publish(contents, 1); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return resolver, versions
}

func TestFallbackToLanguage(t *testing.T) {
	r, _ := setupResolver(t)
	res, err := r.ResolveField("a1", "zh-TW", "title")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Found || res.Locale != "zh" || res.Value != "你好" {
		t.Fatalf("expected fallback to zh, got %+v", res)
	}
	if !res.FellBack {
		t.Fatalf("expected FellBack=true")
	}
}

func TestFallbackToDefault(t *testing.T) {
	r, _ := setupResolver(t)
	// "ja" has no content, falls back to en.
	res, err := r.ResolveField("a1", "ja", "title")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Found || res.Locale != "en" || res.Value != "Hello" {
		t.Fatalf("expected fallback to en, got %+v", res)
	}
}

func TestExplicitEmptyNoFallback(t *testing.T) {
	r, _ := setupResolver(t)
	// subtitle in zh is missing, falls back to en which has it explicitly empty.
	res, err := r.ResolveField("a1", "zh-CN", "subtitle")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Found || !res.Explicit || res.Value != "" {
		t.Fatalf("expected explicit empty, got %+v", res)
	}
	if res.Locale != "en" {
		t.Fatalf("expected locale en, got %s", res.Locale)
	}
}

func TestMissingFieldNotFound(t *testing.T) {
	r, _ := setupResolver(t)
	res, err := r.ResolveField("a1", "en", "nonexistent")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Found {
		t.Fatalf("expected not found, got %+v", res)
	}
}

func TestResolveContent(t *testing.T) {
	r, _ := setupResolver(t)
	res, err := r.ResolveContent("a1", "zh-CN")
	if err != nil {
		t.Fatalf("resolve content: %v", err)
	}
	title := res["title"]
	if title == nil || !title.Found || title.Value != "你好" {
		t.Fatalf("title wrong: %+v", title)
	}
	sub := res["subtitle"]
	if sub == nil || !sub.Found || !sub.Explicit || sub.Value != "" {
		t.Fatalf("subtitle wrong: %+v", sub)
	}
}

func TestFallbackChain(t *testing.T) {
	cfg := config.Default()
	chain := cfg.FallbackChain("zh-CN")
	want := []string{"zh-CN", "zh", "en"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain = %v, want %v", chain, want)
		}
	}
}
