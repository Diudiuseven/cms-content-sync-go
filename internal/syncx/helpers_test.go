package syncx_test

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/Diudiuseven/cms-content-sync-go/internal/config"
	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/publish"
	"github.com/Diudiuseven/cms-content-sync-go/internal/resolve"
	"github.com/Diudiuseven/cms-content-sync-go/internal/schema"
	"github.com/Diudiuseven/cms-content-sync-go/internal/store"
	"github.com/Diudiuseven/cms-content-sync-go/internal/syncx"
	"github.com/Diudiuseven/cms-content-sync-go/internal/upstream"
)

// testEnv bundles the engine and its dependencies for tests.
type testEnv struct {
	src       *upstream.Source
	registry  *schema.Registry
	drafts    *store.DraftStore
	cursors   *store.CursorStore
	versions  *store.VersionStore
	publisher *publish.Publisher
	engine    *syncx.Engine
	resolver  *resolve.Resolver
	cfg       *config.Config
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := config.Default()
	src := upstream.NewSource()
	registry := schema.NewRegistry()
	if err := registry.Register(articleSchema(1)); err != nil {
		t.Fatalf("register schema: %v", err)
	}
	drafts := store.NewDraftStore()
	cursors := store.NewCursorStore()
	versions := store.NewVersionStore()
	publisher := publish.NewPublisher(versions, registry)
	engine := syncx.NewEngine(src, drafts, cursors, publisher, cfg)
	resolver := resolve.NewResolver(versions, cfg)
	return &testEnv{
		src: src, registry: registry, drafts: drafts, cursors: cursors,
		versions: versions, publisher: publisher, engine: engine, resolver: resolver, cfg: cfg,
	}
}

func articleSchema(version int) *model.Schema {
	return &model.Schema{
		ID:      "article",
		Version: version,
		Fields: map[string]model.FieldSchema{
			"title":    {Name: "title", Type: model.TypeString, Required: true, Localized: true},
			"body":     {Name: "body", Type: model.TypeText, Required: false, Localized: true},
			"subtitle": {Name: "subtitle", Type: model.TypeString, Required: false, Localized: true},
		},
	}
}

// seedContent creates a content item with the given title/body.
func seedContent(id, locale, title, body string) *model.Content {
	return &model.Content{
		ID: id, Locale: locale, SchemaID: "article", SchemaVersion: 1,
		Fields: map[string]model.FieldValue{
			"title": {Set: true, Value: title},
			"body":  {Set: true, Value: body},
		},
	}
}

// referenceState applies events in strict sequence order to build the
// expected final state (the "full sync" reference). Deletes are recorded as
// tombstones so the reference matches the engine's internal representation.
func referenceState(events []*model.Event) map[model.ContentKey]*model.Content {
	bySeq := make(map[int64]*model.Event, len(events))
	for _, e := range events {
		bySeq[e.Seq] = e
	}
	out := make(map[model.ContentKey]*model.Content)
	for seq := int64(1); ; seq++ {
		ev, ok := bySeq[seq]
		if !ok {
			break
		}
		key := ev.Key()
		switch ev.Type {
		case model.EventDelete:
			out[key] = &model.Content{
				ID: ev.ID, Locale: ev.Locale, Deleted: true,
				LastSeq: ev.Seq, Fields: make(map[string]model.FieldValue),
			}
		case model.EventUpsert:
			c, ok := out[key]
			if !ok || c.Deleted {
				c = &model.Content{ID: ev.ID, Locale: ev.Locale, Fields: make(map[string]model.FieldValue)}
			}
			for k, v := range ev.Fields {
				c.Fields[k] = v
			}
			c.SchemaID = ev.SchemaID
			c.SchemaVersion = ev.SchemaVersion
			c.Deleted = false
			c.LastSeq = ev.Seq
			out[key] = c
		}
	}
	return out
}

// draftState returns the current drafts as a map for comparison.
func draftState(d *store.DraftStore) map[model.ContentKey]*model.Content {
	return d.Snapshot()
}

// assertContentsEqual fails the test if the two content maps differ.
func assertContentsEqual(t *testing.T, want, got map[model.ContentKey]*model.Content) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("content count mismatch: want %d, got %d", len(want), len(got))
	}
	for k, wc := range want {
		gc, ok := got[k]
		if !ok {
			t.Errorf("missing content %v", k)
			continue
		}
		if !contentEqual(wc, gc) {
			t.Errorf("content %v mismatch:\n want: %+v\n got:  %+v", k, wc, gc)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected content %v", k)
		}
	}
}

func contentEqual(a, b *model.Content) bool {
	if a.Deleted != b.Deleted {
		return false
	}
	if len(a.Fields) != len(b.Fields) {
		return false
	}
	for k, v := range a.Fields {
		w, ok := b.Fields[k]
		if !ok || v.Set != w.Set || v.Value != w.Value {
			return false
		}
	}
	return true
}

// shuffleEvents returns a shuffled copy of the events.
func shuffleEvents(events []*model.Event) []*model.Event {
	out := make([]*model.Event, len(events))
	copy(out, events)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// eventsAfter builds events for the given contents via the upstream source.
func buildEvents(src *upstream.Source, contents []*model.Content) []*model.Event {
	var events []*model.Event
	for _, c := range contents {
		events = append(events, src.Upsert(c))
	}
	return events
}

func formatContents(m map[model.ContentKey]*model.Content) string {
	out := ""
	for k, c := range m {
		out += fmt.Sprintf("  %v: %+v\n", k, c.Fields)
	}
	return out
}
