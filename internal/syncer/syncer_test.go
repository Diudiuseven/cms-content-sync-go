package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/store"
	"github.com/Diudiuseven/cms-content-sync-go/internal/upstream"
)

func testRegistry(t *testing.T) *model.Registry {
	t.Helper()
	r := model.NewRegistry()
	if err := r.Register(model.Schema{
		Name: "article", Version: 1,
		Fields: []model.Field{
			{Name: "title", Type: model.TypeString, Required: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func fields(kv ...string) map[string]json.RawMessage {
	if len(kv)%2 != 0 {
		panic("odd kv")
	}
	out := map[string]json.RawMessage{}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = json.RawMessage(kv[i+1])
	}
	return out
}

func title(s string) map[string]json.RawMessage {
	return fields("title", fmt.Sprintf("%q", s))
}

func newEngine(src upstream.Source, st *store.Store, cp CheckpointStore, attempts, batch int) *Engine {
	return New(src, st, cp, Backoff{
		Base: 1, Max: 4, MaxAttempts: attempts,
		Sleep: func(ctx context.Context, d time.Duration) error { return nil },
	}, batch)
}

// syncAll 循环同步直到游标追上上游位点。
func syncAll(t *testing.T, e *Engine, src upstream.Source) Result {
	t.Helper()
	ctx := context.Background()
	var last Result
	for {
		r, err := e.SyncOnce(ctx)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		last.Applied += r.Applied
		last.Skipped += r.Skipped
		last.Cursor = r.Cursor
		if r.Published != nil {
			last.Published = r.Published
		}
		head, err := src.Head(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r.Cursor.Seq >= head {
			return last
		}
	}
}

func TestIncrementalEqualsFullRebuild(t *testing.T) {
	reg := testRegistry(t)
	src := upstream.NewMemorySource()
	// 制造增删改混合的上游历史。
	src.Apply("a", "article", 1, upstream.ChangeUpsert, title("a1"))
	src.Apply("b", "article", 1, upstream.ChangeUpsert, title("b1"))
	src.Apply("a", "article", 1, upstream.ChangeUpsert, title("a2"))
	src.Apply("c", "article", 1, upstream.ChangeUpsert, title("c1"))
	src.Apply("b", "article", 1, upstream.ChangeDelete, nil)
	src.Apply("a", "article", 1, upstream.ChangeUpsert, title("a3"))

	// 路径一：增量同步（小批次，跨多批）。
	st1 := store.New(reg)
	e1 := newEngine(src, st1, &MemoryCheckpoints{}, 3, 2)
	syncAll(t, e1, src)
	inc := st1.Current()

	// 路径二：完整重建。
	st2 := store.New(reg)
	e2 := newEngine(src, st2, &MemoryCheckpoints{}, 3, 0)
	if err := e2.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	full := st2.Current()

	if inc.Len() != full.Len() {
		t.Fatalf("len %d != %d", inc.Len(), full.Len())
	}
	for id, it := range full.Items() {
		got, ok := inc.Get(id)
		if !ok {
			t.Fatalf("incremental missing %s", id)
		}
		if got.Revision != it.Revision {
			t.Fatalf("%s revision %d != %d", id, got.Revision, it.Revision)
		}
		for k, v := range it.Fields {
			if string(got.Fields[k]) != string(v) {
				t.Fatalf("%s.%s = %s != %s", id, k, got.Fields[k], v)
			}
		}
	}
	if _, ok := inc.Get("b"); ok {
		t.Fatal("deleted item b must stay deleted")
	}
}

func TestTransientFailureRetryAndCursorStability(t *testing.T) {
	reg := testRegistry(t)
	src := upstream.NewMemorySource()
	src.Apply("a", "article", 1, upstream.ChangeUpsert, title("a1"))
	src.Apply("b", "article", 1, upstream.ChangeUpsert, title("b1"))

	// 注入 2 次瞬时失败，重试上限 3：应成功。
	src.InjectFailures(upstream.FailurePlan{FailFetch: 2})
	st := store.New(reg)
	cp := &MemoryCheckpoints{}
	e := newEngine(src, st, cp, 3, 0)
	r, err := e.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("bounded retry should succeed: %v", err)
	}
	if r.Applied != 2 || r.Cursor.Seq != 2 {
		t.Fatalf("result = %+v", r)
	}

	// 注入超过上限的失败：报错且不留部分写入、游标不动。
	src.Apply("c", "article", 1, upstream.ChangeUpsert, title("c1"))
	src.InjectFailures(upstream.FailurePlan{FailFetch: 5})
	before := st.DraftItems()
	if _, err := e.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	cur, _ := e.Cursor(context.Background())
	if cur.Seq != 2 {
		t.Fatalf("cursor moved to %d on failure", cur.Seq)
	}
	after := st.DraftItems()
	if len(after) != len(before) {
		t.Fatalf("partial write leaked: %d != %d", len(after), len(before))
	}
}

func TestPartialFetchLeavesNoPartialWrites(t *testing.T) {
	reg := testRegistry(t)
	src := upstream.NewMemorySource()
	for i := 0; i < 6; i++ {
		src.Apply(fmt.Sprintf("id%d", i), "article", 1, upstream.ChangeUpsert, title(fmt.Sprintf("t%d", i)))
	}
	// 第一次 Fetch 返回部分数据 + 错误：引擎必须整批丢弃并重试。
	src.InjectFailures(upstream.FailurePlan{PartialFetch: 1})
	st := store.New(reg)
	e := newEngine(src, st, &MemoryCheckpoints{}, 3, 0)
	r, err := e.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("retry after partial failure: %v", err)
	}
	if r.Applied != 6 {
		t.Fatalf("applied = %d, want 6", r.Applied)
	}
	if st.Current().Len() != 6 {
		t.Fatalf("published len = %d", st.Current().Len())
	}
}

func TestCrashRecoveryFromCheckpoint(t *testing.T) {
	reg := testRegistry(t)
	src := upstream.NewMemorySource()
	for i := 0; i < 5; i++ {
		src.Apply(fmt.Sprintf("id%d", i), "article", 1, upstream.ChangeUpsert, title(fmt.Sprintf("t%d", i)))
	}
	st := store.New(reg)
	cp := &MemoryCheckpoints{}

	// 引擎一：每批 2 条，同步一批后“崩溃”（丢弃引擎，保留检查点与存储）。
	e1 := newEngine(src, st, cp, 3, 2)
	if _, err := e1.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	cur, _ := cp.Load(context.Background())
	if cur.Seq != 2 {
		t.Fatalf("checkpoint = %d, want 2", cur.Seq)
	}

	// 引擎二：从检查点恢复，重复处理不得产生副作用。
	e2 := newEngine(src, st, cp, 3, 2)
	syncAll(t, e2, src)
	if got := st.Current().Len(); got != 5 {
		t.Fatalf("len = %d, want 5", got)
	}
	// 再次重放已提交的位点之前的数据：游标语义保证不会重复拉取；
	// 即使强制重放（新检查点从 0 开始），修订号去重也保证幂等。
	cp2 := &MemoryCheckpoints{}
	e3 := newEngine(src, st, cp2, 3, 0)
	r, err := e3.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Applied != 0 {
		t.Fatalf("reprocessing must be side-effect free, applied = %d", r.Applied)
	}
	if r.Skipped != 5 {
		t.Fatalf("skipped = %d, want 5", r.Skipped)
	}
	if got := st.Current().Len(); got != 5 {
		t.Fatalf("len after replay = %d", got)
	}
}

func TestRebuildPicksUpChangesArrivingDuringRebuild(t *testing.T) {
	reg := testRegistry(t)
	src := upstream.NewMemorySource()
	src.Apply("a", "article", 1, upstream.ChangeUpsert, title("a1"))
	src.Apply("b", "article", 1, upstream.ChangeUpsert, title("b1"))

	st := store.New(reg)
	e := newEngine(src, st, &MemoryCheckpoints{}, 3, 0)
	syncAll(t, e, src)
	before := st.Current()

	// 重建前/期间到达的新变更（在快照位点之后）必须出现在重建结果里。
	src.Apply("c", "article", 1, upstream.ChangeUpsert, title("c1"))
	src.Apply("a", "article", 1, upstream.ChangeUpsert, title("a2"))
	src.Apply("b", "article", 1, upstream.ChangeDelete, nil)

	if err := e.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := st.Current()

	// 对齐：重建后的对外内容 = 重建前内容 + 期间变更。
	if after.Number <= before.Number {
		t.Fatalf("version did not advance: %d -> %d", before.Number, after.Number)
	}
	if _, ok := after.Get("c"); !ok {
		t.Fatal("change during rebuild lost: c missing")
	}
	if _, ok := after.Get("b"); ok {
		t.Fatal("delete during rebuild lost: b still present")
	}
	a, _ := after.Get("a")
	if string(a.Fields["title"]) != `"a2"` {
		t.Fatalf("a.title = %s, want a2", a.Fields["title"])
	}
	// 可解释：新版本的 AtSeq 等于上游当前位点。
	head, _ := src.Head(context.Background())
	if after.AtSeq != head {
		t.Fatalf("AtSeq = %d, head = %d", after.AtSeq, head)
	}
	// 重建前的旧版本仍可查询。
	old, ok := st.Version(before.Number)
	if !ok || old.Len() != 2 {
		t.Fatalf("old version lost: ok=%v", ok)
	}
}

func TestConcurrentRebuildSyncAndReads(t *testing.T) {
	reg := testRegistry(t)
	src := upstream.NewMemorySource()
	for i := 0; i < 10; i++ {
		src.Apply(fmt.Sprintf("id%d", i), "article", 1, upstream.ChangeUpsert, title(fmt.Sprintf("t%d", i)))
	}
	st := store.New(reg)
	e := newEngine(src, st, &MemoryCheckpoints{}, 3, 3)

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 4)

	// 增量同步。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			if _, err := e.SyncOnce(ctx); err != nil {
				errs <- fmt.Errorf("sync: %w", err)
				return
			}
		}
	}()
	// 并发重建。
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := e.Rebuild(ctx); err != nil {
			errs <- fmt.Errorf("rebuild: %w", err)
		}
	}()
	// 并发读取：任何时刻只能读到完整版本。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			v := st.Current()
			items := v.Items()
			for id, it := range items {
				if _, err := v.Get(id); !err {
					errs <- fmt.Errorf("version %d missing %s", v.Number, id)
					return
				}
				if it.Revision == 0 {
					errs <- fmt.Errorf("zero revision in version %d", v.Number)
					return
				}
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// 收敛：最终对外内容与上游一致。
	syncAll(t, e, src)
	if got := st.Current().Len(); got != 10 {
		t.Fatalf("final len = %d, want 10", got)
	}
}

func TestModelEvolutionRoundTripAndCoexistence(t *testing.T) {
	reg := testRegistry(t)
	// 演进：v2 新增可选字段 summary。
	if err := reg.Register(model.Schema{
		Name: "article", Version: 2,
		Fields: []model.Field{
			{Name: "title", Type: model.TypeString, Required: true},
			{Name: "summary", Type: model.TypeString},
		},
	}); err != nil {
		t.Fatal(err)
	}

	src := upstream.NewMemorySource()
	// 旧模型内容：带模型未声明的字段（未来字段），往返必须原样保留。
	src.Apply("old", "article", 1, upstream.ChangeUpsert,
		fields("title", `"old"`, "x_future", `{"k":[1,2]}`))
	// 新模型内容。
	src.Apply("new", "article", 2, upstream.ChangeUpsert,
		fields("title", `"new"`, "summary", `"s"`))

	st := store.New(reg)
	e := newEngine(src, st, &MemoryCheckpoints{}, 3, 0)
	syncAll(t, e, src)

	cur := st.Current()
	oldIt, ok := cur.Get("old")
	if !ok {
		t.Fatal("old-model item missing")
	}
	if string(oldIt.Fields["x_future"]) != `{"k":[1,2]}` {
		t.Fatalf("unknown field not preserved: %s", oldIt.Fields["x_future"])
	}
	if oldIt.ModelVer != 1 {
		t.Fatalf("old item model version = %d", oldIt.ModelVer)
	}
	newIt, ok := cur.Get("new")
	if !ok || newIt.ModelVer != 2 {
		t.Fatalf("new-model item: %+v ok=%v", newIt, ok)
	}
	if string(newIt.Fields["summary"]) != `"s"` {
		t.Fatalf("summary = %s", newIt.Fields["summary"])
	}
	// 新旧模型内容在同一份存储、同一个发布版本里共存。
	if cur.Len() != 2 {
		t.Fatalf("len = %d", cur.Len())
	}
}

func TestPoisonChangeDoesNotAdvanceCursor(t *testing.T) {
	reg := testRegistry(t)
	src := upstream.NewMemorySource()
	src.Apply("ok", "article", 1, upstream.ChangeUpsert, title("ok"))
	// 毒消息：缺必填字段。
	src.Apply("bad", "article", 1, upstream.ChangeUpsert, fields("other", `"x"`))

	st := store.New(reg)
	e := newEngine(src, st, &MemoryCheckpoints{}, 3, 0)
	if _, err := e.SyncOnce(context.Background()); err == nil {
		t.Fatal("expected validation error")
	}
	cur, _ := e.Cursor(context.Background())
	if cur.Seq != 0 {
		t.Fatalf("cursor advanced to %d on poison batch", cur.Seq)
	}
	if got := len(st.DraftItems()); got != 0 {
		t.Fatalf("partial write leaked: %d items", got)
	}
}
