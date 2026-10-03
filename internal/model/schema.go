// Package model 定义内容模型（schema）、模型注册表与兼容性校验。
//
// 模型演进规则：
//   - 新增字段（无论可选/必填中的可选）属于兼容变更，直接接受；
//   - 新增必填字段视为不兼容（旧内容无法满足），按 RemovedRequiredField 之外的
//     独立原因拒绝并不在本实现支持范围内——本实现仅允许新增可选字段；
//   - 删除必需字段、疑似改名必需字段、字段类型变更属于不兼容变更，
//     以 *IncompatibleError 拒绝，原因可通过 IncompatibilityReason 区分；
//   - 内容中出现模型未声明的字段是允许的，并在存储与往返中原样保留。
package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// FieldType 是字段的标量类型。
type FieldType string

const (
	TypeString FieldType = "string"
	TypeNumber FieldType = "number"
	TypeBool   FieldType = "bool"
	TypeObject FieldType = "object"
)

// Field 描述内容模型中的一个字段。
type Field struct {
	Name      string
	Type      FieldType
	Required  bool
	Localized bool
}

// Schema 是内容模型的一个具名版本。
type Schema struct {
	Name    string
	Version int
	Fields  []Field
}

// field 按名索引字段。
func (s Schema) field(name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// IncompatibilityReason 区分不兼容变更的具体原因。
type IncompatibilityReason int

const (
	// ReasonRemovedRequiredField 必需字段被删除。
	ReasonRemovedRequiredField IncompatibilityReason = iota + 1
	// ReasonRenamedRequiredField 必需字段疑似被改名（同类型新字段出现）。
	ReasonRenamedRequiredField
	// ReasonFieldTypeChanged 已存在字段的类型发生变化。
	ReasonFieldTypeChanged
	// ReasonAddedRequiredField 新增必需字段（旧内容将无法满足约束）。
	ReasonAddedRequiredField
)

func (r IncompatibilityReason) String() string {
	switch r {
	case ReasonRemovedRequiredField:
		return "removed-required-field"
	case ReasonRenamedRequiredField:
		return "renamed-required-field"
	case ReasonFieldTypeChanged:
		return "field-type-changed"
	case ReasonAddedRequiredField:
		return "added-required-field"
	default:
		return "unknown"
	}
}

// IncompatibleError 表示一次被拒绝的不兼容模型演进。
type IncompatibleError struct {
	Schema string
	Reason IncompatibilityReason
	Field  string
	Detail string
}

func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("model %q: incompatible change (%s) on field %q: %s",
		e.Schema, e.Reason, e.Field, e.Detail)
}

// ValidationError 表示内容不满足其模型约束。
type ValidationError struct {
	Model  string
	Field  string
	Detail string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("model %q: invalid field %q: %s", e.Model, e.Field, e.Detail)
}

// Registry 保存同一模型多个版本的 schema，并负责兼容性检查。
type Registry struct {
	schemas map[string][]Schema // name -> 按 Version 升序
}

func NewRegistry() *Registry { return &Registry{schemas: map[string][]Schema{}} }

// Register 注册新 schema；若与同名旧版本不兼容则返回 *IncompatibleError。
func (r *Registry) Register(s Schema) error {
	if s.Name == "" {
		return fmt.Errorf("model: schema name must not be empty")
	}
	prev, ok := r.Latest(s.Name)
	if ok {
		if s.Version <= prev.Version {
			return fmt.Errorf("model %q: version %d must be greater than %d", s.Name, s.Version, prev.Version)
		}
		if err := checkCompatible(prev, s); err != nil {
			return err
		}
	} else if s.Version < 1 {
		return fmt.Errorf("model %q: version must be >= 1", s.Name)
	}
	r.schemas[s.Name] = append(r.schemas[s.Name], s)
	return nil
}

// checkCompatible 校验 next 相对 prev 是否向后兼容。
func checkCompatible(prev, next Schema) error {
	// 1) 已有字段：类型不得变化；必需字段不得消失（消失且存在同类型新字段视为改名）。
	for _, of := range prev.Fields {
		nf, ok := next.field(of.Name)
		if ok {
			if nf.Type != of.Type {
				return &IncompatibleError{
					Schema: next.Name, Reason: ReasonFieldTypeChanged, Field: of.Name,
					Detail: fmt.Sprintf("type changed from %s to %s", of.Type, nf.Type),
				}
			}
			continue
		}
		if !of.Required {
			continue // 可选字段允许删除
		}
		// 必需字段消失：检查是否有同类型的新增字段，有则判定为疑似改名。
		for _, nf := range next.Fields {
			if _, existed := prev.field(nf.Name); !existed && nf.Type == of.Type {
				return &IncompatibleError{
					Schema: next.Name, Reason: ReasonRenamedRequiredField, Field: of.Name,
					Detail: fmt.Sprintf("required field removed while new field %q of same type %s appeared", nf.Name, nf.Type),
				}
			}
		}
		return &IncompatibleError{
			Schema: next.Name, Reason: ReasonRemovedRequiredField, Field: of.Name,
			Detail: "required field was removed",
		}
	}
	// 2) 新增字段：不允许新增必需字段（存量旧内容无法满足）。
	for _, nf := range next.Fields {
		if _, existed := prev.field(nf.Name); !existed && nf.Required {
			return &IncompatibleError{
				Schema: next.Name, Reason: ReasonAddedRequiredField, Field: nf.Name,
				Detail: "adding a new required field breaks existing content",
			}
		}
	}
	return nil
}

// Get 返回指定模型指定版本的 schema。
func (r *Registry) Get(name string, version int) (Schema, bool) {
	for _, s := range r.schemas[name] {
		if s.Version == version {
			return s, true
		}
	}
	return Schema{}, false
}

// Latest 返回指定模型的最新版本。
func (r *Registry) Latest(name string) (Schema, bool) {
	vs := r.schemas[name]
	if len(vs) == 0 {
		return Schema{}, false
	}
	return vs[len(vs)-1], true
}

// Versions 返回某模型已注册的全部版本号（升序）。
func (r *Registry) Versions(name string) []int {
	out := make([]int, 0, len(r.schemas[name]))
	for _, s := range r.schemas[name] {
		out = append(out, s.Version)
	}
	sort.Ints(out)
	return out
}

// Validate 校验字段集合是否满足 schema 约束；未知字段允许存在并被保留。
func (s Schema) Validate(fields map[string]json.RawMessage) error {
	for _, f := range s.Fields {
		raw, ok := fields[f.Name]
		if !ok {
			if f.Required {
				return &ValidationError{Model: s.Name, Field: f.Name, Detail: "required field missing"}
			}
			continue
		}
		if f.Localized {
			// 本地化字段是 {locale: value} 对象，每个值须匹配字段类型。
			var lv map[string]json.RawMessage
			if err := json.Unmarshal(raw, &lv); err != nil {
				return &ValidationError{Model: s.Name, Field: f.Name,
					Detail: "localized field must be an object of locale -> value"}
			}
			for loc, val := range lv {
				if err := CheckType(f.Type, val); err != nil {
					return &ValidationError{Model: s.Name, Field: f.Name,
						Detail: fmt.Sprintf("locale %q: %v", loc, err)}
				}
			}
			continue
		}
		if err := CheckType(f.Type, raw); err != nil {
			return &ValidationError{Model: s.Name, Field: f.Name, Detail: err.Error()}
		}
	}
	return nil
}

// CheckType 校验单个值是否匹配字段类型。
func CheckType(t FieldType, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("empty value")
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return nil // null 对任何类型都接受（视为未设置）
	}
	switch t {
	case TypeString:
		var v string
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return fmt.Errorf("expected string")
		}
	case TypeNumber:
		var v json.Number
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return fmt.Errorf("expected number")
		}
	case TypeBool:
		var v bool
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return fmt.Errorf("expected bool")
		}
	case TypeObject:
		var v map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return fmt.Errorf("expected object")
		}
	default:
		return fmt.Errorf("unknown field type %q", t)
	}
	return nil
}
