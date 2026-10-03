package syncx_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/schema"
	"github.com/Diudiuseven/cms-content-sync-go/internal/syncx"
	"github.com/Diudiuseven/cms-content-sync-go/internal/upstream"
)

// TestOutOfOrderDelivery verifies that delivering events in a shuffled order
// produces exactly the same final state as a full sync.
func TestOutOfOrderDelivery(t *testing.T) {
	env := newTestEnv(t)
	contents := []*model.Content{
		seedContent("a1", "en", "Hello", "World"),
		seedContent("a2", "en", "Foo", "Bar"),
		seedContent("a1", "zh", "你好", "世界"),
	}
	events := buildEvents(env.src, contents)
	want := referenceState(events)

	shuffled := shuffleEvents(events)
	for _, ev := range shuffled {
		env.engine.Deliver(ev)
	}

	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
	if env.engine.Cursor() != int64(len(events)) {
		t.Fatalf("cursor = %d, want %d", env.engine.Cursor(), len(events))
	}
}

// TestDuplicateDelivery verifies that delivering the same event multiple times
// has no side effects.
func TestDuplicateDelivery(t *testing.T) {
	env := newTestEnv(t)
	contents := []*model.Content{
		seedContent("a1", "en", "Hello", "World"),
		seedContent("a2", "en", "Foo", "Bar"),
	}
	events := buildEvents(env.src, contents)
	want := referenceState(events)

	// Deliver each event three times.
	for _, ev := range events {
		for i := 0; i < 3; i++ {
			env.engine.Deliver(ev)
		}
	}

	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
	if env.engine.Cursor() != int64(len(events)) {
		t.Fatalf("cursor = %d, want %d", env.engine.Cursor(), len(events))
	}
}

// TestLateArrival verifies that an event arriving after the cursor has
// advanced past it is ignored (no rollback, no side effects).
func TestLateArrival(t *testing.T) {
	env := newTestEnv(t)
	contents := []*model.Content{
		seedContent("a1", "en", "Hello", "World"),
		seedContent("a2", "en", "Foo", "Bar"),
		seedContent("a3", "en", "Baz", "Qux"),
	}
	events := buildEvents(env.src, contents)
	want := referenceState(events)

	// Deliver all events in order.
	for _, ev := range events {
		env.engine.Deliver(ev)
	}
	// Now deliver a late duplicate of event 1 (already applied).
	env.engine.Deliver(events[0])

	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
	if env.engine.Cursor() != 3 {
		t.Fatalf("cursor = %d, want 3", env.engine.Cursor())
	}
}

// TestDeleteOutOfOrder verifies that a delete arriving before an upsert with a
// higher sequence is handled correctly (final state = full sync).
func TestDeleteOutOfOrder(t *testing.T) {
	env := newTestEnv(t)
	// Upsert a1 (seq 1), upsert a1 again (seq 2), delete a1 (seq 3).
	ev1 := env.src.Upsert(seedContent("a1", "en", "V1", "B1"))
	ev2 := env.src.Upsert(seedContent("a1", "en", "V2", "B2"))
	ev3 := env.src.Delete("a1", "en")
	events := []*model.Event{ev1, ev2, ev3}
	want := referenceState(events)

	// Deliver in reverse order.
	for i := len(events) - 1; i >= 0; i-- {
		env.engine.Deliver(events[i])
	}

	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
}

// TestCompatibleModelEvolution verifies that adding a new field is accepted and
// the new field is preserved through round-trip.
func TestCompatibleModelEvolution(t *testing.T) {
	env := newTestEnv(t)
	// Register schema v2 with an added optional field "subtitle".
	schemaV2 := articleSchema(2)
	if err := env.registry.Register(schemaV2); err != nil {
		t.Fatalf("register v2: %v", err)
	}

	// Content using v2 with the new field.
	c := seedContent("a1", "en", "Hello", "World")
	c.SchemaVersion = 2
	c.Fields["subtitle"] = model.FieldValue{Set: true, Value: "Sub"}
	ev := env.src.Upsert(c)
	env.engine.Deliver(ev)

	got := draftState(env.drafts)
	if got[model.ContentKey{ID: "a1", Locale: "en"}].Fields["subtitle"].Value != "Sub" {
		t.Fatalf("new field not preserved: %+v", got)
	}
	if got[model.ContentKey{ID: "a1", Locale: "en"}].SchemaVersion != 2 {
		t.Fatalf("schema version not preserved")
	}
}

// TestIncompatibleModelEvolution verifies that removing or renaming a required
// field is rejected with a distinguishable reason.
func TestIncompatibleModelEvolution(t *testing.T) {
	env := newTestEnv(t)

	tests := []struct {
		name   string
		schema *model.Schema
		reason schema.IncompatibilityReason
	}{
		{
			name: "remove required field",
			schema: &model.Schema{
				ID: "article", Version: 2,
				Fields: map[string]model.FieldSchema{
					"body": {Name: "body", Type: model.TypeText, Required: false},
				},
			},
			reason: schema.ReasonRequiredFieldRemoved,
		},
		{
			name: "rename required field",
			schema: &model.Schema{
				ID: "article", Version: 2,
				Fields: map[string]model.FieldSchema{
					"headline": {Name: "headline", Type: model.TypeString, Required: true},
					"body":     {Name: "body", Type: model.TypeText, Required: false},
				},
			},
			reason: schema.ReasonRequiredFieldRemoved,
		},
		{
			name: "change required field type",
			schema: &model.Schema{
				ID: "article", Version: 2,
				Fields: map[string]model.FieldSchema{
					"title": {Name: "title", Type: model.TypeInt, Required: true},
					"body":  {Name: "body", Type: model.TypeText, Required: false},
				},
			},
			reason: schema.ReasonTypeChanged,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := env.registry.Register(tc.schema)
			if err == nil {
				t.Fatal("expected rejection, got nil")
			}
			var incompat *schema.IncompatibilityError
			if !errors.As(err, &incompat) {
				t.Fatalf("expected IncompatibilityError, got %T: %v", err, err)
			}
			if incompat.Reason != tc.reason {
				t.Fatalf("reason = %s, want %s", incompat.Reason, tc.reason)
			}
		})
	}
}

// TestOldAndNewModelCoexist verifies that content written against different
// schema versions coexist in the same store.
func TestOldAndNewModelCoexist(t *testing.T) {
	env := newTestEnv(t)
	if err := env.registry.Register(articleSchema(2)); err != nil {
		t.Fatalf("register v2: %v", err)
	}

	c1 := seedContent("a1", "en", "Old", "Body") // v1
	c1.SchemaVersion = 1
	c2 := seedContent("a2", "en", "New", "Body") // v2 with subtitle
	c2.SchemaVersion = 2
	c2.Fields["subtitle"] = model.FieldValue{Set: true, Value: "Sub"}

	events := buildEvents(env.src, []*model.Content{c1, c2})
	for _, ev := range events {
		env.engine.Deliver(ev)
	}

	got := draftState(env.drafts)
	if got[model.ContentKey{ID: "a1", Locale: "en"}].SchemaVersion != 1 {
		t.Fatalf("a1 schema version = %d, want 1", got[model.ContentKey{ID: "a1", Locale: "en"}].SchemaVersion)
	}
	if got[model.ContentKey{ID: "a2", Locale: "en"}].SchemaVersion != 2 {
		t.Fatalf("a2 schema version = %d, want 2", got[model.ContentKey{ID: "a2", Locale: "en"}].SchemaVersion)
	}
	if got[model.ContentKey{ID: "a2", Locale: "en"}].Fields["subtitle"].Value != "Sub" {
		t.Fatalf("a2 subtitle not preserved")
	}
}

// TestLocaleFallback verifies deterministic fallback and the distinction
// between fallback values and explicitly empty values.
func TestLocaleFallback(t *testing.T) {
	env := newTestEnv(t)

	// English: title set, subtitle explicitly empty.
	en := seedContent("a1", "en", "Hello", "World")
	en.Fields["subtitle"] = model.FieldValue{Set: true, Value: ""}
	// Chinese: title set, no subtitle.
	zh := seedContent("a1", "zh", "你好", "世界")
	// Chinese (Taiwan): nothing set.
	events := buildEvents(env.src, []*model.Content{en, zh})
	for _, ev := range events {
		env.engine.Deliver(ev)
	}
	if _, err := env.engine.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// title in zh-CN: falls back to zh (has title).
	res, err := env.resolver.ResolveField("a1", "zh-CN", "title")
	if err != nil {
		t.Fatalf("resolve title: %v", err)
	}
	if !res.Found || res.Locale != "zh" || res.Value != "你好" {
		t.Fatalf("title fallback wrong: %+v", res)
	}

	// subtitle in zh-CN: zh has no subtitle, falls back to en which has it
	// explicitly empty. Must return found + explicit empty, NOT fall through.
	res, err = env.resolver.ResolveField("a1", "zh-CN", "subtitle")
	if err != nil {
		t.Fatalf("resolve subtitle: %v", err)
	}
	if !res.Found || !res.Explicit || res.Value != "" {
		t.Fatalf("subtitle explicit empty not distinguished: %+v", res)
	}
	if res.Locale != "en" {
		t.Fatalf("subtitle locale = %s, want en", res.Locale)
	}

	// A field that is never set anywhere: not found.
	res, err = env.resolver.ResolveField("a1", "zh-CN", "nonexistent")
	if err != nil {
		t.Fatalf("resolve nonexistent: %v", err)
	}
	if res.Found {
		t.Fatalf("nonexistent field should not be found: %+v", res)
	}
}

// TestExplicitEmptyNoFallback verifies that an explicitly empty value in the
// requested locale does not trigger further fallback.
func TestExplicitEmptyNoFallback(t *testing.T) {
	env := newTestEnv(t)
	// zh has subtitle explicitly set to empty.
	zh := seedContent("a1", "zh", "你好", "世界")
	zh.Fields["subtitle"] = model.FieldValue{Set: true, Value: ""}
	en := seedContent("a1", "en", "Hello", "World")
	en.Fields["subtitle"] = model.FieldValue{Set: true, Value: "English Sub"}
	events := buildEvents(env.src, []*model.Content{zh, en})
	for _, ev := range events {
		env.engine.Deliver(ev)
	}
	if _, err := env.engine.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	res, err := env.resolver.ResolveField("a1", "zh-CN", "subtitle")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Found || !res.Explicit || res.Value != "" || res.Locale != "zh" {
		t.Fatalf("explicit empty should not fall back: %+v", res)
	}
}

// TestConcurrentRebuild verifies that a rebuild running concurrently with
// incremental sync does not lose changes.
func TestConcurrentRebuild(t *testing.T) {
	env := newTestEnv(t)

	// Seed initial content.
	initial := []*model.Content{
		seedContent("a1", "en", "V0", "B0"),
		seedContent("a2", "en", "V0", "B0"),
	}
	events := buildEvents(env.src, initial)
	for _, ev := range events {
		env.engine.Deliver(ev)
	}

	// Concurrently deliver new events and rebuild.
	var wg sync.WaitGroup
	errCh := make(chan error, 10)

	// Producer: deliver updates to a1 and a2.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 20; i++ {
			c := seedContent("a1", "en", fmt.Sprintf("V%d", i), fmt.Sprintf("B%d", i))
			ev := env.src.Upsert(c)
			env.engine.Deliver(ev)
		}
	}()

	// Rebuilder: rebuild repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			if _, err := env.engine.Rebuild(); err != nil {
				errCh <- err
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent rebuild error: %v", err)
	}

	// Final rebuild to align.
	if _, err := env.engine.Rebuild(); err != nil {
		t.Fatalf("final rebuild: %v", err)
	}

	// Verify drafts match the upstream full state.
	want := env.src.AllContents()
	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
}

// TestInterruptRecovery verifies that the engine resumes from the last
// committed cursor after a restart and that duplicate replay has no effect.
func TestInterruptRecovery(t *testing.T) {
	env := newTestEnv(t)

	// First run: sync events 1..3.
	first := []*model.Content{
		seedContent("a1", "en", "V1", "B1"),
		seedContent("a2", "en", "V2", "B2"),
		seedContent("a3", "en", "V3", "B3"),
	}
	events1 := buildEvents(env.src, first)
	for _, ev := range events1 {
		env.engine.Deliver(ev)
	}
	if _, err := env.engine.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	cursorAfterFirst := env.engine.Cursor()
	if cursorAfterFirst != 3 {
		t.Fatalf("cursor after first = %d, want 3", cursorAfterFirst)
	}

	// Simulate restart: new engine, same persisted stores.
	env2 := &testEnv{
		src: env.src, registry: env.registry, drafts: env.drafts,
		cursors: env.cursors, versions: env.versions, publisher: env.publisher,
		cfg: env.cfg,
	}
	env2.engine = syncx.NewEngine(env.src, env.drafts, env.cursors, env.publisher, env.cfg)
	env2.engine.Start()
	defer env2.engine.Stop()

	// Re-deliver events 1..3 (duplicate replay) - must have no effect.
	for _, ev := range events1 {
		env2.engine.Deliver(ev)
	}
	if env2.engine.Cursor() != 3 {
		t.Fatalf("cursor after duplicate replay = %d, want 3", env2.engine.Cursor())
	}

	// Sync events 4..6.
	second := []*model.Content{
		seedContent("a4", "en", "V4", "B4"),
		seedContent("a5", "en", "V5", "B5"),
		seedContent("a6", "en", "V6", "B6"),
	}
	events2 := buildEvents(env.src, second)
	for _, ev := range events2 {
		env2.engine.Deliver(ev)
	}

	want := referenceState(append(events1, events2...))
	got := draftState(env2.drafts)
	assertContentsEqual(t, want, got)
	if env2.engine.Cursor() != 6 {
		t.Fatalf("cursor = %d, want 6", env2.engine.Cursor())
	}
}

// TestRetryBoundedBackoff verifies that transient failures are retried with
// bounded backoff and eventually succeed, leaving no partial writes.
func TestRetryBoundedBackoff(t *testing.T) {
	env := newTestEnv(t)

	// Inject failures: fail requests for events with seq >= 2, up to 3 times.
	env.src.SetFailure(upstream.FailTransient, 2, 3)

	contents := []*model.Content{
		seedContent("a1", "en", "V1", "B1"),
		seedContent("a2", "en", "V2", "B2"),
		seedContent("a3", "en", "V3", "B3"),
	}
	events := buildEvents(env.src, contents)
	want := referenceState(events)

	// SyncOnce should retry and succeed.
	n, err := env.engine.SyncOnce()
	if err != nil {
		t.Fatalf("SyncOnce after retries: %v", err)
	}
	if n != 3 {
		t.Fatalf("synced %d events, want 3", n)
	}

	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
	if env.engine.Cursor() != 3 {
		t.Fatalf("cursor = %d, want 3", env.engine.Cursor())
	}
}

// TestRetryExhausted verifies that persistent failure returns an error and
// leaves no partial writes.
func TestRetryExhausted(t *testing.T) {
	env := newTestEnv(t)
	// Fail forever (more than MaxRetries).
	env.src.SetFailure(upstream.FailTransient, 1, 100)

	contents := []*model.Content{
		seedContent("a1", "en", "V1", "B1"),
	}
	buildEvents(env.src, contents)

	_, err := env.engine.SyncOnce()
	if err == nil {
		t.Fatal("expected error after retry exhaustion, got nil")
	}
	// No partial writes: drafts must be empty.
	if env.drafts.Len() != 0 {
		t.Fatalf("partial writes left in drafts: %d entries", env.drafts.Len())
	}
	if env.engine.Cursor() != 0 {
		t.Fatalf("cursor = %d, want 0 (no commit)", env.engine.Cursor())
	}
}

// TestRebuildRetry verifies that a transient snapshot failure during rebuild
// is retried and eventually succeeds, leaving no partial state.
func TestRebuildRetry(t *testing.T) {
	env := newTestEnv(t)
	contents := []*model.Content{
		seedContent("a1", "en", "V1", "B1"),
		seedContent("a2", "en", "V2", "B2"),
	}
	events := buildEvents(env.src, contents)
	for _, ev := range events {
		env.engine.Deliver(ev)
	}

	// Fail snapshot requests up to 2 times.
	env.src.SetFailure(upstream.FailTransient, 1, 2)

	report, err := env.engine.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild after retries: %v", err)
	}
	if report.ContentsTotal != 2 {
		t.Fatalf("contents total = %d, want 2", report.ContentsTotal)
	}
	want := env.src.AllContents()
	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
}

// TestAtomicPublish verifies that readers always see a complete version.
func TestAtomicPublish(t *testing.T) {
	env := newTestEnv(t)
	contents := []*model.Content{
		seedContent("a1", "en", "V1", "B1"),
		seedContent("a2", "en", "V2", "B2"),
	}
	events := buildEvents(env.src, contents)
	for _, ev := range events {
		env.engine.Deliver(ev)
	}

	v1, err := env.engine.Publish()
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}

	// Update a1 and publish again.
	ev3 := env.src.Upsert(seedContent("a1", "en", "V3", "B3"))
	env.engine.Deliver(ev3)
	v2, err := env.engine.Publish()
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	// v1 must still show V1 (no field-level mixing).
	gotV1, ok := v1.Get(model.ContentKey{ID: "a1", Locale: "en"})
	if !ok {
		t.Fatal("a1 missing from v1")
	}
	if gotV1.Fields["title"].Value != "V1" {
		t.Fatalf("v1 a1 title = %v, want V1", gotV1.Fields["title"].Value)
	}

	// v2 must show V3.
	gotV2, ok := v2.Get(model.ContentKey{ID: "a1", Locale: "en"})
	if !ok {
		t.Fatal("a1 missing from v2")
	}
	if gotV2.Fields["title"].Value != "V3" {
		t.Fatalf("v2 a1 title = %v, want V3", gotV2.Fields["title"].Value)
	}

	// Historical versions remain queryable.
	if env.versions.LatestNumber() != 2 {
		t.Fatalf("latest version = %d, want 2", env.versions.LatestNumber())
	}
}

// TestRebuildAlignment verifies that a rebuild with no changes produces an
// aligned report and that the published content is unchanged.
func TestRebuildAlignment(t *testing.T) {
	env := newTestEnv(t)
	contents := []*model.Content{
		seedContent("a1", "en", "V1", "B1"),
		seedContent("a2", "en", "V2", "B2"),
	}
	events := buildEvents(env.src, contents)
	for _, ev := range events {
		env.engine.Deliver(ev)
	}
	if _, err := env.engine.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	report, err := env.engine.Rebuild()
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !report.Aligned {
		t.Fatalf("rebuild not aligned: added=%v removed=%v changed=%v",
			report.Added, report.Removed, report.Changed)
	}
	if report.BeforeVersion != 1 || report.AfterVersion != 2 {
		t.Fatalf("versions = %d->%d, want 1->2", report.BeforeVersion, report.AfterVersion)
	}
}

// TestRebuildWithChanges verifies that a rebuild incorporating upstream changes
// reports the diff and publishes the new state.
func TestRebuildWithChanges(t *testing.T) {
	env := newTestEnv(t)
	contents := []*model.Content{
		seedContent("a1", "en", "V1", "B1"),
	}
	events := buildEvents(env.src, contents)
	for _, ev := range events {
		env.engine.Deliver(ev)
	}
	if _, err := env.engine.Publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Upstream changes after the initial sync.
	env.src.Upsert(seedContent("a1", "en", "V2", "B2"))
	env.src.Upsert(seedContent("a2", "en", "New", "Body"))

	report, err := env.engine.Rebuild()
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if report.Aligned {
		t.Fatal("rebuild should not be aligned after upstream changes")
	}
	if len(report.Changed) != 1 || report.Changed[0].ID != "a1" {
		t.Fatalf("changed = %v, want [a1]", report.Changed)
	}
	if len(report.Added) != 1 || report.Added[0].ID != "a2" {
		t.Fatalf("added = %v, want [a2]", report.Added)
	}

	// Verify published content matches upstream.
	want := env.src.AllContents()
	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
}

// TestSyncOncePull verifies the pull-based sync path.
func TestSyncOncePull(t *testing.T) {
	env := newTestEnv(t)
	contents := []*model.Content{
		seedContent("a1", "en", "V1", "B1"),
		seedContent("a2", "en", "V2", "B2"),
	}
	events := buildEvents(env.src, contents)
	want := referenceState(events)

	n, err := env.engine.SyncOnce()
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("synced %d, want 2", n)
	}
	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
}

// TestPullPathWithReordering verifies that the engine handles network
// reordering on the pull path (events returned out of order) and still
// produces the correct final state.
func TestPullPathWithReordering(t *testing.T) {
	env := newTestEnv(t)
	env.src.SetShuffle(true)

	// Build a mix of upserts and a delete via the source.
	env.src.Upsert(seedContent("a1", "en", "V1", "B1"))
	env.src.Upsert(seedContent("a2", "en", "V2", "B2"))
	env.src.Upsert(seedContent("a1", "en", "V3", "B3"))
	env.src.Upsert(seedContent("a3", "en", "V4", "B4"))
	env.src.Delete("a2", "en")

	events, _ := env.src.EventsAfter(0)
	want := referenceState(events)

	n, err := env.engine.SyncOnce()
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if n != len(events) {
		t.Fatalf("synced %d, want %d", n, len(events))
	}
	got := draftState(env.drafts)
	assertContentsEqual(t, want, got)
}

// TestStartStopLifecycle verifies that Start loads the persisted cursor and
// the background loop syncs and auto-publishes.
func TestStartStopLifecycle(t *testing.T) {
	env := newTestEnv(t)
	env.cfg.PublishInterval = 10 * time.Millisecond

	// Seed upstream before starting.
	env.src.Upsert(seedContent("a1", "en", "V1", "B1"))

	env.engine.Start()
	defer env.engine.Stop()

	// Wait for the background loop to sync and publish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if env.versions.LatestNumber() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if env.versions.LatestNumber() < 1 {
		t.Fatal("background loop did not publish a version")
	}
	if env.engine.Cursor() != 1 {
		t.Fatalf("cursor = %d, want 1", env.engine.Cursor())
	}

	// Start again should be idempotent.
	env.engine.Start()
}

// TestPublishInvalidContent verifies that publishing invalid content is
// rejected and no version is created.
func TestPublishInvalidContent(t *testing.T) {
	env := newTestEnv(t)
	// Content missing required field "title".
	c := &model.Content{
		ID: "a1", Locale: "en", SchemaID: "article", SchemaVersion: 1,
		Fields: map[string]model.FieldValue{
			"body": {Set: true, Value: "no title"},
		},
	}
	ev := env.src.Upsert(c)
	env.engine.Deliver(ev)

	_, err := env.engine.Publish()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if env.versions.LatestNumber() != 0 {
		t.Fatalf("version created despite invalid content: %d", env.versions.LatestNumber())
	}
}
