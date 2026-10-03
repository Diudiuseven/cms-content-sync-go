// Package store 提供草稿/发布分离的版本化内容存储。
//
// 语义：
//   - 同步引擎只写草稿（Tx 或 ReplaceDraft），读取方只能看到已发布版本；
//   - Publish 把当前草稿整体固化为不可变 Version 并原子切换，
//     任何读取方要么看到旧版本、要么看到新版本，不会出现字段级混合；
//   - 历史版本全部保留，可按版本号查询；
//   - 乱序/重复/迟到的变更通过“每内容单调修订号 + 删除墓碑”收敛，
//     保证最终结果等价于一次完整同步。
package store

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/Diudiuseven/cms-content-sync-go/internal/model"
)

// Item 是一条已存储的内容。
type Item struct {
	ID       string
	Model    string
	ModelVer int
	Revision uint64
	Fields   map[string]json.RawMessage
}

// clone 深拷贝内容，保证版本不可变。
func (it Item) clone() Item {
	out := it
	if it.Fields != nil {
		out.Fields = make(map[string]json.RawMessage, len(it.Fields))
		for k, v := range it.Fields {
			cp := make([]byte, len(v))
			copy(cp, v)
			out.Fields[k] = cp
		}
	}
	return out
}

// Version 是一个不可变的已发布快照；读取方持有它即可看到一致的完整版本。
type Version struct {
	Number uint64
	AtSeq  uint64 // 发布时同步到的上游位点
	items  map[string]Item
}

// Get 返回该版本中的一条内容（拷贝）。
func (v *Version) Get(id string) (Item, bool) {
	it, ok := v.items[id]
	if !ok {
		return Item{}, false
	}
	return it.clone(), true
}

// Items 返回该版本全部内容的拷贝。
func (v *Version) Items() map[string]Item {
	out := make(map[string]Item, len(v.items))
	for k, it := range v.items {
		out[k] = it.clone()
	}
	return out
}

// Len 返回内容条数。
func (v *Version) Len() int { return len(v.items) }

// Store 是版本化内容存储：草稿可写，发布原子切换，历史可查。
type Store struct {
	reg *model.Registry

	mu       sync.Mutex // 保护草稿、墓碑与历史
	draft    map[string]Item
	tomb     map[string]uint64 // 删除墓碑：ID -> 删除时的修订号
	current  atomic.Pointer[Version]
	versions []*Version
}

func New(reg *model.Registry) *Store {
	s := &Store{
		reg:   reg,
		draft: map[string]Item{},
		tomb:  map[string]uint64{},
	}
	// 版本 0：空发布，保证 Current 永不返回 nil。
	v0 := &Version{Number: 0, items: map[string]Item{}}
	s.versions = append(s.versions, v0)
	s.current.Store(v0)
	return s
}

// Stats 记录一次事务提交的生效情况。
type Stats struct {
	Applied int
	Skipped int // 因重复/迟到被忽略的操作数
}

// Tx 是一组待提交的草稿修改；要么全部生效，要么全部丢弃。
type Tx struct {
	s       *Store
	upserts []Item
	deletes []del
}

type del struct {
	id       string
	revision uint64
}

// Begin 开启一个草稿事务。
func (s *Store) Begin() *Tx { return &Tx{s: s} }

// Upsert 暂存一次写入。
func (t *Tx) Upsert(it Item) { t.upserts = append(t.upserts, it) }

// Delete 暂存一次删除。
func (t *Tx) Delete(id string, revision uint64) {
	t.deletes = append(t.deletes, del{id: id, revision: revision})
}

// Commit 校验并原子提交事务；任一校验失败则整体丢弃，不留部分写入。
// 提交时按修订号去重：不高于已存修订号（或墓碑）的操作被忽略。
func (t *Tx) Commit() (Stats, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()

	// 先整体校验，全部通过才落写。
	for _, it := range t.upserts {
		sch, ok := t.s.reg.Get(it.Model, it.ModelVer)
		if !ok {
			return Stats{}, fmt.Errorf("store: unknown model %q version %d", it.Model, it.ModelVer)
		}
		if err := sch.Validate(it.Fields); err != nil {
			return Stats{}, err
		}
	}

	var st Stats
	for _, it := range t.upserts {
		if t.s.tomb[it.ID] >= it.Revision {
			st.Skipped++ // 比已应用的删除更旧：迟到
			continue
		}
		if cur, ok := t.s.draft[it.ID]; ok && cur.Revision >= it.Revision {
			st.Skipped++ // 重复或乱序的旧修订
			continue
		}
		t.s.draft[it.ID] = it.clone()
		delete(t.s.tomb, it.ID)
		st.Applied++
	}
	for _, d := range t.deletes {
		if t.s.tomb[d.id] >= d.revision {
			st.Skipped++
			continue
		}
		if cur, ok := t.s.draft[d.id]; ok && cur.Revision >= d.revision {
			st.Skipped++
			continue
		}
		delete(t.s.draft, d.id)
		t.s.tomb[d.id] = d.revision
		st.Applied++
	}
	return st, nil
}

// ReplaceDraft 用全量快照整体替换草稿（用于完整重建）；校验失败则草稿保持不变。
func (s *Store) ReplaceDraft(items []Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]Item, len(items))
	for _, it := range items {
		sch, ok := s.reg.Get(it.Model, it.ModelVer)
		if !ok {
			return fmt.Errorf("store: unknown model %q version %d", it.Model, it.ModelVer)
		}
		if err := sch.Validate(it.Fields); err != nil {
			return err
		}
		next[it.ID] = it.clone()
	}
	s.draft = next
	s.tomb = map[string]uint64{} // 快照即真相，墓碑随之失效
	return nil
}

// Publish 将当前草稿发布为新的不可变版本并原子切换。
func (s *Store) Publish(atSeq uint64) *Version {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make(map[string]Item, len(s.draft))
	for k, it := range s.draft {
		items[k] = it.clone()
	}
	v := &Version{
		Number: uint64(len(s.versions)),
		AtSeq:  atSeq,
		items:  items,
	}
	s.versions = append(s.versions, v)
	s.current.Store(v)
	return v
}

// Current 返回当前已发布版本；读取方看到的是完整一致的快照。
func (s *Store) Current() *Version { return s.current.Load() }

// Version 查询历史版本。
func (s *Store) Version(n uint64) (*Version, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= uint64(len(s.versions)) {
		return nil, false
	}
	return s.versions[n], true
}

// Versions 返回全部已发布版本号（升序）。
func (s *Store) Versions() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, len(s.versions))
	for i, v := range s.versions {
		out[i] = v.Number
	}
	return out
}

// DraftItems 返回草稿内容拷贝（测试与重建对齐用）。
func (s *Store) DraftItems() map[string]Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Item, len(s.draft))
	for k, it := range s.draft {
		out[k] = it.clone()
	}
	return out
}
