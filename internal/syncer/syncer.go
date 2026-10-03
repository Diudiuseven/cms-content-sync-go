// Package syncer 实现基于游标的增量同步、有界退避重试与完整重建。
//
// 关键语义：
//   - 游标（Cursor.Seq）表达“已提交处理到”的上游位点，只有在一批变更
//     完整落库并发布后才推进，崩溃后从最近提交位置继续；
//   - 一批变更在一个事务中校验并提交，任何失败都不会留下部分写入；
//     重复处理同一批变更因修订号去重而无副作用（幂等）；
//   - 上游瞬时失败按有界指数退避重试，超过上限后报错且游标不前进；
//   - 完整重建以快照为基底，回放快照之后到达的变更，再原子发布，
//     重建期间的新变更不会丢失。
package syncer

import (
	"context"
	"sync"
	"time"

	"github.com/Diudiuseven/cms-content-sync-go/internal/store"
	"github.com/Diudiuseven/cms-content-sync-go/internal/upstream"
)

// Cursor 表达增量同步已处理到的位置。
type Cursor struct {
	Seq uint64
}

// CheckpointStore 持久化游标，崩溃后从最近提交位置恢复。
type CheckpointStore interface {
	Load(ctx context.Context) (Cursor, error)
	Save(ctx context.Context, c Cursor) error
}

// MemoryCheckpoints 是 CheckpointStore 的内存实现。
type MemoryCheckpoints struct {
	mu sync.Mutex
	c  Cursor
}

func (m *MemoryCheckpoints) Load(ctx context.Context) (Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c, nil
}

func (m *MemoryCheckpoints) Save(ctx context.Context, c Cursor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.c = c
	return nil
}

// Backoff 是有界退避策略：间隔从 Base 起指数翻倍、封顶 Max，最多尝试 MaxAttempts 次。
type Backoff struct {
	Base        time.Duration                                    // 初始退避
	Max         time.Duration                                    // 单次退避上限
	MaxAttempts int                                              // 最大尝试次数（含首次）
	Sleep       func(ctx context.Context, d time.Duration) error // 可注入；nil 使用真实睡眠
}

func (b Backoff) wait(ctx context.Context, d time.Duration) error {
	if b.Sleep != nil {
		return b.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Result 描述一次同步批次的结果。
type Result struct {
	Applied   int            // 实际生效的变更数
	Skipped   int            // 因重复/迟到被跳过的变更数
	Cursor    Cursor         // 批次提交后的游标
	Published *store.Version // 内容有变化时发布的新版本；无变化为 nil
}

// Engine 编排增量同步、重建与发布。
type Engine struct {
	src     upstream.Source
	st      *store.Store
	cp      CheckpointStore
	backoff Backoff
	batch   int

	mu sync.Mutex // 串行化 SyncOnce 与 Rebuild（写路径）；读路径无锁
}

func New(src upstream.Source, st *store.Store, cp CheckpointStore, b Backoff, batchSize int) *Engine {
	if b.MaxAttempts <= 0 {
		b.MaxAttempts = 1
	}
	if b.Max <= 0 {
		b.Max = b.Base
	}
	return &Engine{src: src, st: st, cp: cp, backoff: b, batch: batchSize}
}

// Cursor 返回最近提交的游标。
func (e *Engine) Cursor(ctx context.Context) (Cursor, error) { return e.cp.Load(ctx) }

// withRetry 按有界指数退避重试 fn。
func (e *Engine) withRetry(ctx context.Context, fn func() error) error {
	d := e.backoff.Base
	var err error
	for attempt := 1; attempt <= e.backoff.MaxAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt == e.backoff.MaxAttempts {
			break
		}
		if werr := e.backoff.wait(ctx, d); werr != nil {
			return werr
		}
		d *= 2
		if d > e.backoff.Max {
			d = e.backoff.Max
		}
	}
	return err
}

// SyncOnce 拉取并应用一批变更；失败按有界退避重试，不产生部分写入。
func (e *Engine) SyncOnce(ctx context.Context) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	cur, err := e.cp.Load(ctx)
	if err != nil {
		return Result{}, err
	}

	var changes []upstream.Change
	err = e.withRetry(ctx, func() error {
		var ferr error
		changes, ferr = e.src.Fetch(ctx, cur.Seq, e.batch)
		return ferr
	})
	if err != nil {
		// 重试耗尽：未写入任何内容，游标保持不动。
		return Result{}, err
	}
	if len(changes) == 0 {
		return Result{Cursor: cur}, nil
	}

	tx := e.st.Begin()
	for _, c := range changes {
		switch c.Type {
		case upstream.ChangeUpsert:
			tx.Upsert(store.Item{
				ID: c.ID, Model: c.Model, ModelVer: c.ModelVer,
				Revision: c.Revision, Fields: c.Fields,
			})
		case upstream.ChangeDelete:
			tx.Delete(c.ID, c.Revision)
		}
	}
	stats, err := tx.Commit()
	if err != nil {
		// 校验失败：整批丢弃，游标不前进（毒消息由上层处理）。
		return Result{}, err
	}

	next := Cursor{Seq: changes[len(changes)-1].Seq}
	if err := e.cp.Save(ctx, next); err != nil {
		return Result{}, err
	}

	res := Result{Applied: stats.Applied, Skipped: stats.Skipped, Cursor: next}
	if stats.Applied > 0 {
		res.Published = e.st.Publish(next.Seq)
	}
	return res, nil
}

// Rebuild 完整重建：以快照为基底、回放快照之后到达的变更，然后原子发布。
// 重建期间旧版本持续对外可读；新变更通过回放与游标语义保证不丢失。
func (e *Engine) Rebuild(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var snap []upstream.Change
	var atSeq uint64
	err := e.withRetry(ctx, func() error {
		var serr error
		snap, atSeq, serr = e.src.Snapshot(ctx)
		return serr
	})
	if err != nil {
		return err
	}

	items := make([]store.Item, 0, len(snap))
	for _, c := range snap {
		items = append(items, store.Item{
			ID: c.ID, Model: c.Model, ModelVer: c.ModelVer,
			Revision: c.Revision, Fields: c.Fields,
		})
	}
	if err := e.st.ReplaceDraft(items); err != nil {
		return err
	}

	// 回放快照位点之后到达的变更（重建期间的新变更不丢失）。
	cursor := Cursor{Seq: atSeq}
	if ms, ok := e.src.(interface {
		ChangesSince(uint64) []upstream.Change
	}); ok {
		replay := ms.ChangesSince(atSeq)
		if len(replay) > 0 {
			tx := e.st.Begin()
			for _, c := range replay {
				switch c.Type {
				case upstream.ChangeUpsert:
					tx.Upsert(store.Item{
						ID: c.ID, Model: c.Model, ModelVer: c.ModelVer,
						Revision: c.Revision, Fields: c.Fields,
					})
				case upstream.ChangeDelete:
					tx.Delete(c.ID, c.Revision)
				}
			}
			if _, err := tx.Commit(); err != nil {
				return err
			}
			cursor.Seq = replay[len(replay)-1].Seq
		}
	}

	e.st.Publish(cursor.Seq)
	return e.cp.Save(ctx, cursor)
}
