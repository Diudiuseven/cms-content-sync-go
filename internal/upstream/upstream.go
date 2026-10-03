// Package upstream 用内存实现模拟外部内容源及其变更流。
//
// MemorySource 维护一条全局单调递增的变更日志与每内容的修订号，
// 支持注入瞬时失败与部分失败，行为完全确定、可在本地复现。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrTransient 表示可重试的瞬时失败。
var ErrTransient = errors.New("upstream: transient failure")

// ChangeType 区分变更种类。
type ChangeType int

const (
	ChangeUpsert ChangeType = iota + 1
	ChangeDelete
)

func (t ChangeType) String() string {
	switch t {
	case ChangeUpsert:
		return "upsert"
	case ChangeDelete:
		return "delete"
	default:
		return "unknown"
	}
}

// Change 是上游变更日志中的一条记录。
type Change struct {
	Seq      uint64                     // 全局单调递增位点
	ID       string                     // 内容 ID
	Type     ChangeType                 // 变更种类
	Model    string                     // 内容模型名
	ModelVer int                        // 内容模型版本
	Revision uint64                     // 该内容的单调递增修订号（用于乱序/去重判定）
	Fields   map[string]json.RawMessage // Upsert 时的完整字段集
}

// Source 抽象外部内容源。
type Source interface {
	// Fetch 返回 seq 严格大于 after 的一批变更（按 seq 升序）；limit<=0 表示不限。
	Fetch(ctx context.Context, after uint64, limit int) ([]Change, error)
	// Snapshot 返回全量内容（以最新修订的 Upsert 表示）及其对应的日志位点。
	Snapshot(ctx context.Context) (items []Change, atSeq uint64, err error)
	// Head 返回当前日志最大位点。
	Head(ctx context.Context) (uint64, error)
}

// FailurePlan 描述注入的瞬时失败。
type FailurePlan struct {
	FailFetch    int // 接下来 Fetch 调用失败的次数
	FailSnapshot int // 接下来 Snapshot 调用失败的次数
	PartialFetch int // 接下来 Fetch 调用返回部分数据后报错的次数
}

// MemorySource 是 Source 的内存实现，支持失败注入，行为确定可复现。
type MemorySource struct {
	mu   sync.Mutex
	log  []Change          // 按 Seq 升序
	rev  map[string]uint64 // ID -> 最新修订号
	plan FailurePlan
}

func NewMemorySource() *MemorySource {
	return &MemorySource{rev: map[string]uint64{}}
}

// Apply 追加一条变更（模拟上游增删改），返回分配的位点。
// fields 会被深拷贝，调用方后续修改不影响日志。
func (m *MemorySource) Apply(id, model string, modelVer int, typ ChangeType, fields map[string]json.RawMessage) Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rev[id]++
	seq := uint64(len(m.log)) + 1
	c := Change{
		Seq:      seq,
		ID:       id,
		Type:     typ,
		Model:    model,
		ModelVer: modelVer,
		Revision: m.rev[id],
		Fields:   cloneFields(fields),
	}
	m.log = append(m.log, c)
	return c
}

// InjectFailures 设置失败注入计划。
func (m *MemorySource) InjectFailures(p FailurePlan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plan = p
}

func (m *MemorySource) Fetch(ctx context.Context, after uint64, limit int) ([]Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.plan.FailFetch > 0 {
		m.plan.FailFetch--
		return nil, fmt.Errorf("fetch: %w", ErrTransient)
	}
	var out []Change
	for _, c := range m.log {
		if c.Seq > after {
			out = append(out, cloneChange(c))
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if m.plan.PartialFetch > 0 && len(out) > 1 {
		m.plan.PartialFetch--
		// 部分失败：返回一半数据并附带错误；调用方必须整批丢弃。
		return out[:len(out)/2], fmt.Errorf("fetch: partial result: %w", ErrTransient)
	}
	return out, nil
}

func (m *MemorySource) Snapshot(ctx context.Context) ([]Change, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.plan.FailSnapshot > 0 {
		m.plan.FailSnapshot--
		return nil, 0, fmt.Errorf("snapshot: %w", ErrTransient)
	}
	// 以每 ID 最新一条 Upsert 表示全量现状；已删除内容不出现。
	latest := map[string]Change{}
	deleted := map[string]bool{}
	for _, c := range m.log {
		if c.Type == ChangeDelete {
			deleted[c.ID] = true
			delete(latest, c.ID)
			continue
		}
		latest[c.ID] = c
		delete(deleted, c.ID)
	}
	out := make([]Change, 0, len(latest))
	for _, c := range latest {
		out = append(out, cloneChange(c))
	}
	// 按 ID 排序保证输出确定。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, uint64(len(m.log)), nil
}

func (m *MemorySource) Head(ctx context.Context) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return uint64(len(m.log)), nil
}

// ChangesSince 返回 seq 严格大于 after 的全部变更（供重建回放使用）。
func (m *MemorySource) ChangesSince(after uint64) []Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Change
	for _, c := range m.log {
		if c.Seq > after {
			out = append(out, cloneChange(c))
		}
	}
	return out
}

func cloneFields(in map[string]json.RawMessage) map[string]json.RawMessage {
	if in == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

func cloneChange(c Change) Change {
	c.Fields = cloneFields(c.Fields)
	return c
}
