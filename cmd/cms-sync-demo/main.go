// cms-sync-demo 在单进程内演示完整链路：增量同步 → 校验 → 版本化发布 → 重建，
// 全程使用内存模拟的上游与检查点，输出确定、可在本地稳定复现。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/Diudiuseven/cms-content-sync-go/internal/locale"
	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
	"github.com/Diudiuseven/cms-content-sync-go/internal/store"
	"github.com/Diudiuseven/cms-content-sync-go/internal/syncer"
	"github.com/Diudiuseven/cms-content-sync-go/internal/upstream"
)

func main() {
	ctx := context.Background()

	// 1. 注册内容模型（含一次兼容演进：v2 新增可选字段 summary）。
	reg := model.NewRegistry()
	must(reg.Register(model.Schema{
		Name: "article", Version: 1,
		Fields: []model.Field{
			{Name: "title", Type: model.TypeString, Required: true, Localized: true},
			{Name: "body", Type: model.TypeString, Required: true, Localized: true},
		},
	}))
	must(reg.Register(model.Schema{
		Name: "article", Version: 2,
		Fields: []model.Field{
			{Name: "title", Type: model.TypeString, Required: true, Localized: true},
			{Name: "body", Type: model.TypeString, Required: true, Localized: true},
			{Name: "summary", Type: model.TypeString},
		},
	}))

	// 2. 模拟上游：持续增删改，并注入一次瞬时失败。
	src := upstream.NewMemorySource()
	src.Apply("hello", "article", 1, upstream.ChangeUpsert, map[string]json.RawMessage{
		"title": json.RawMessage(`{"en":"Hello","de":"Hallo"}`),
		"body":  json.RawMessage(`{"en":"World"}`),
	})
	src.Apply("draft-note", "article", 2, upstream.ChangeUpsert, map[string]json.RawMessage{
		"title":   json.RawMessage(`{"en":"Note"}`),
		"body":    json.RawMessage(`{"en":"..."}`),
		"summary": json.RawMessage(`"short"`),
	})
	src.InjectFailures(upstream.FailurePlan{FailFetch: 1}) // 第一次拉取会瞬时失败

	// 3. 同步引擎：有界退避重试 + 检查点。
	st := store.New(reg)
	cp := &syncer.MemoryCheckpoints{}
	eng := syncer.New(src, st, cp, syncer.Backoff{
		Base: 1, Max: 4, MaxAttempts: 3,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}, 0)

	syncAll(ctx, eng, src)
	fmt.Printf("[sync] published version=%d items=%d\n",
		st.Current().Number, st.Current().Len())

	// 4. 上游继续变化：修改 + 删除，再增量同步。
	src.Apply("hello", "article", 1, upstream.ChangeUpsert, map[string]json.RawMessage{
		"title": json.RawMessage(`{"en":"Hello v2","de":""}`), // de 显式置空
		"body":  json.RawMessage(`{"en":"World v2"}`),
	})
	src.Apply("draft-note", "article", 2, upstream.ChangeDelete, nil)
	syncAll(ctx, eng, src)
	cur := st.Current()
	fmt.Printf("[sync] published version=%d items=%d atSeq=%d\n",
		cur.Number, cur.Len(), cur.AtSeq)

	// 5. 多语言回退：de 显式空 vs 缺失回退。
	it, _ := cur.Get("hello")
	var titles locale.Values
	must(json.Unmarshal(it.Fields["title"], &titles))
	chain := locale.Chain{"de-AT", "de", "en"}
	r := titles.Resolve(chain)
	fmt.Printf("[locale] title(de-AT)=%q explicit=%v from=%s\n", r.Value, r.Explicit, r.Locale)

	// 6. 完整重建：与增量结果对齐。
	must(eng.Rebuild(ctx))
	fmt.Printf("[rebuild] published version=%d items=%d atSeq=%d\n",
		st.Current().Number, st.Current().Len(), st.Current().AtSeq)

	// 7. 历史版本查询。
	fmt.Printf("[history] versions=%v\n", st.Versions())
}

func syncAll(ctx context.Context, eng *syncer.Engine, src upstream.Source) {
	for {
		r, err := eng.SyncOnce(ctx)
		if err != nil {
			log.Fatalf("sync failed: %v", err)
		}
		head, err := src.Head(ctx)
		if err != nil {
			log.Fatalf("head: %v", err)
		}
		if r.Cursor.Seq >= head {
			return
		}
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
