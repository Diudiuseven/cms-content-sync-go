package store

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
)

func testRegistry(t *testing.T) *model.Registry {
	t.Helper()
	r := model.NewRegistry()
	err := r.Register(model.Schema{
		Name: "article", Version: 1,
		Fields: []model.Field{
			{Name: "title", Type: model.TypeString, Required: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func item(id string, rev uint64, title string) Item {
	return Item{
		ID: id, Model: "article", ModelVer: 1, Revision: rev,
		Fields: map[string]json.RawMessage{"title": json.RawMessage(fmt.Sprintf("%q", title))},
	}
}

func TestPublishAtomicAndHistory(t *testing.T) {
	s := New(testRegistry(t))

	tx := s.Begin()
	tx.Upsert(item("a", 1, "v1"))
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	v1 := s.Publish(1)

	// 发布期间/之后，读取方只能看到某个完整版本。
	tx2 := s.Begin()
	tx2.Upsert(item("a", 2, "v2"))
	tx2.Upsert(item("b", 1, "b1"))
	if _, err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	// 未发布前，Current 仍是 v1。
	if got := s.Current(); got.Number != v1.Number {
		t.Fatalf("current = %d before publish", got.Number)
	}
	v2 := s.Publish(2)

	cur := s.Current()
	if cur.Number != v2.Number || cur.Len() != 2 {
		t.Fatalf("current = %+v", cur)
	}
	a, _ := cur.Get("a")
	if string(a.Fields["title"]) != `"v2"` {
		t.Fatalf("a.title = %s", a.Fields["title"])
	}
	// 历史版本仍可查询且内容不变。
	old, ok := s.Version(v1.Number)
	if !ok {
		t.Fatal("v1 not found")
	}
	oa, _ := old.Get("a")
	if string(oa.Fields["title"]) != `"v1"` {
		t.Fatalf("v1 a.title = %s", oa.Fields["title"])
	}
	if _, ok := old.Get("b"); ok {
		t.Fatal("v1 must not contain b")
	}
	if got := s.Versions(); len(got) != 3 { // 0,1,2
		t.Fatalf("versions = %v", got)
	}
}

func TestTxCommitAllOrNothing(t *testing.T) {
	s := New(testRegistry(t))
	tx := s.Begin()
	tx.Upsert(item("a", 1, "ok"))
	// 混入一条校验失败的写入：缺必填字段。
	tx.Upsert(Item{ID: "bad", Model: "article", ModelVer: 1, Revision: 1,
		Fields: map[string]json.RawMessage{}})
	if _, err := tx.Commit(); err == nil {
		t.Fatal("expected validation error")
	}
	// 整批丢弃：草稿里不能有 a。
	if _, ok := s.DraftItems()["a"]; ok {
		t.Fatal("partial write leaked into draft")
	}
}

func TestOutOfOrderDuplicateLate(t *testing.T) {
	s := New(testRegistry(t))
	tx := s.Begin()
	// 乱序：rev3 先于 rev2 到达。
	tx.Upsert(item("a", 3, "third"))
	tx.Upsert(item("a", 2, "second"))
	// 重复：rev3 再来一次。
	tx.Upsert(item("a", 3, "third"))
	// 删除后迟到的旧更新不得复活内容。
	tx.Delete("a", 4)
	tx.Upsert(item("a", 2, "second"))
	st, err := tx.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if st.Applied != 2 || st.Skipped != 3 {
		t.Fatalf("stats = %+v", st)
	}
	if _, ok := s.DraftItems()["a"]; ok {
		t.Fatal("late upsert resurrected deleted item")
	}
	// 删除之后更新的修订仍可生效（新一次创建）。
	tx2 := s.Begin()
	tx2.Upsert(item("a", 5, "reborn"))
	if _, err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	a := s.DraftItems()["a"]
	if string(a.Fields["title"]) != `"reborn"` {
		t.Fatalf("a.title = %s", a.Fields["title"])
	}
}

func TestConcurrentReadersSeeConsistentVersions(t *testing.T) {
	s := New(testRegistry(t))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 读取方：任何时刻读到的版本内所有内容必须来自同一版本。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				v := s.Current()
				items := v.Items()
				// 版本内一致性：条数与内容必须自洽（同一快照）。
				for id, it := range items {
					again, ok := v.Get(id)
					if !ok || again.Revision != it.Revision {
						t.Errorf("inconsistent read of %s in version %d", id, v.Number)
						return
					}
				}
			}
		}()
	}
	// 写方：反复修改并发布。
	for r := 0; r < 50; r++ {
		tx := s.Begin()
		tx.Upsert(item("a", uint64(r+1), fmt.Sprintf("v%d", r)))
		tx.Upsert(item("b", uint64(r+1), fmt.Sprintf("w%d", r)))
		if _, err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		s.Publish(uint64(r + 1))
	}
	close(stop)
	wg.Wait()
	if s.Current().Len() != 2 {
		t.Fatalf("final len = %d", s.Current().Len())
	}
}

func TestReplaceDraftValidationFailureKeepsOld(t *testing.T) {
	s := New(testRegistry(t))
	tx := s.Begin()
	tx.Upsert(item("a", 1, "keep"))
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	bad := []Item{{ID: "x", Model: "article", ModelVer: 1, Revision: 1,
		Fields: map[string]json.RawMessage{}}}
	if err := s.ReplaceDraft(bad); err == nil {
		t.Fatal("expected error")
	}
	if _, ok := s.DraftItems()["a"]; !ok {
		t.Fatal("failed ReplaceDraft must not alter draft")
	}
}
