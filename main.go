package main

import (
	"fmt"
	"math/rand"
	"os"

	"github.com/Diudiuseven/cms-content-sync-go/internal/config"
	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/publish"
	"github.com/Diudiuseven/cms-content-sync-go/internal/resolve"
	"github.com/Diudiuseven/cms-content-sync-go/internal/schema"
	"github.com/Diudiuseven/cms-content-sync-go/internal/store"
	"github.com/Diudiuseven/cms-content-sync-go/internal/syncx"
	"github.com/Diudiuseven/cms-content-sync-go/internal/upstream"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Default()

	src := upstream.NewSource()
	registry := schema.NewRegistry()
	if err := registry.Register(&model.Schema{
		ID: "article", Version: 1,
		Fields: map[string]model.FieldSchema{
			"title": {Name: "title", Type: model.TypeString, Required: true, Localized: true},
			"body":  {Name: "body", Type: model.TypeText, Required: false, Localized: true},
		},
	}); err != nil {
		return err
	}

	drafts := store.NewDraftStore()
	cursors := store.NewCursorStore()
	versions := store.NewVersionStore()
	publisher := publish.NewPublisher(versions, registry)
	engine := syncx.NewEngine(src, drafts, cursors, publisher, cfg)
	resolver := resolve.NewResolver(versions, cfg)

	fmt.Println("== 1. Incremental sync with out-of-order delivery ==")
	contents := []*model.Content{
		content("a1", "en", "Hello", "World", 1),
		content("a2", "en", "Foo", "Bar", 1),
		content("a1", "zh", "你好", "世界", 1),
	}
	var events []*model.Event
	for _, c := range contents {
		events = append(events, src.Upsert(c))
	}
	// Shuffle to simulate network reordering.
	rand.Shuffle(len(events), func(i, j int) { events[i], events[j] = events[j], events[i] })
	for _, ev := range events {
		engine.Deliver(ev)
	}
	// Duplicate delivery: no side effects.
	engine.Deliver(events[0])
	fmt.Printf("cursor=%d, drafts=%d (duplicate delivery had no effect)\n", engine.Cursor(), drafts.Len())

	fmt.Println("\n== 2. Atomic versioned publish ==")
	v1, err := engine.Publish()
	if err != nil {
		return err
	}
	fmt.Printf("published version %d, cursor=%d\n", v1.Number, v1.Cursor)

	fmt.Println("\n== 3. Locale fallback (missing vs explicit empty) ==")
	// Add an explicit empty subtitle in English.
	en := content("a1", "en", "Hello", "World", 1)
	en.Fields["subtitle"] = model.FieldValue{Set: true, Value: ""}
	engine.Deliver(src.Upsert(en))
	if _, err := engine.Publish(); err != nil {
		return err
	}
	titleRes, _ := resolver.ResolveField("a1", "zh-CN", "title")
	fmt.Printf("title zh-CN: found=%v locale=%s value=%q\n", titleRes.Found, titleRes.Locale, titleRes.Value)
	subRes, _ := resolver.ResolveField("a1", "zh-CN", "subtitle")
	fmt.Printf("subtitle zh-CN: found=%v explicit=%v locale=%s value=%q (explicit empty, no further fallback)\n",
		subRes.Found, subRes.Explicit, subRes.Locale, subRes.Value)

	fmt.Println("\n== 4. Content model evolution ==")
	// Compatible: add optional field.
	if err := registry.Register(&model.Schema{
		ID: "article", Version: 2,
		Fields: map[string]model.FieldSchema{
			"title":    {Name: "title", Type: model.TypeString, Required: true, Localized: true},
			"body":     {Name: "body", Type: model.TypeText, Required: false, Localized: true},
			"subtitle": {Name: "subtitle", Type: model.TypeString, Required: false, Localized: true},
		},
	}); err != nil {
		return err
	}
	fmt.Println("registered schema v2 (added subtitle): compatible")
	// Incompatible: remove required field.
	err = registry.Register(&model.Schema{
		ID: "article", Version: 3,
		Fields: map[string]model.FieldSchema{
			"body": {Name: "body", Type: model.TypeText, Required: false, Localized: true},
		},
	})
	fmt.Printf("register schema v3 (removed required title): rejected -> %v\n", err)

	fmt.Println("\n== 5. Full rebuild ==")
	report, err := engine.Rebuild()
	if err != nil {
		return err
	}
	fmt.Printf("rebuild: version %d->%d, cursor %d->%d, aligned=%v\n",
		report.BeforeVersion, report.AfterVersion, report.BeforeCursor, report.AfterCursor, report.Aligned)

	fmt.Println("\n== 6. Transient failure with bounded backoff ==")
	src.SetFailure(upstream.FailTransient, 1, 2)
	if _, err := engine.SyncOnce(); err != nil {
		return err
	}
	fmt.Printf("sync recovered after retries, cursor=%d\n", engine.Cursor())

	fmt.Println("\nAll scenarios completed successfully.")
	return nil
}

func content(id, locale, title, body string, schemaVersion int) *model.Content {
	return &model.Content{
		ID: id, Locale: locale, SchemaID: "article", SchemaVersion: schemaVersion,
		Fields: map[string]model.FieldValue{
			"title": {Set: true, Value: title},
			"body":  {Set: true, Value: body},
		},
	}
}
