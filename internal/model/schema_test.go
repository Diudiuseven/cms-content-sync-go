package model

import (
	"encoding/json"
	"errors"
	"testing"
)

func articleV1() Schema {
	return Schema{
		Name: "article", Version: 1,
		Fields: []Field{
			{Name: "title", Type: TypeString, Required: true, Localized: true},
			{Name: "body", Type: TypeString, Required: true, Localized: true},
			{Name: "views", Type: TypeNumber},
		},
	}
}

func TestRegisterAcceptsCompatibleEvolution(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(articleV1()); err != nil {
		t.Fatal(err)
	}
	v2 := articleV1()
	v2.Version = 2
	v2.Fields = append(v2.Fields, Field{Name: "summary", Type: TypeString}) // 新增可选字段
	if err := r.Register(v2); err != nil {
		t.Fatalf("compatible evolution rejected: %v", err)
	}
	if got := r.Versions("article"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("versions: %v", got)
	}
}

func TestRegisterRejectsIncompatible(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(s *Schema)
		reason IncompatibilityReason
	}{
		{"remove required", func(s *Schema) {
			s.Fields = s.Fields[1:] // 删掉 title
		}, ReasonRemovedRequiredField},
		{"rename required", func(s *Schema) {
			s.Fields[0].Name = "headline" // title -> headline，同类型
		}, ReasonRenamedRequiredField},
		{"type change", func(s *Schema) {
			s.Fields[2].Type = TypeString // views: number -> string
		}, ReasonFieldTypeChanged},
		{"add required", func(s *Schema) {
			s.Fields = append(s.Fields, Field{Name: "slug", Type: TypeString, Required: true})
		}, ReasonAddedRequiredField},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(articleV1()); err != nil {
				t.Fatal(err)
			}
			v2 := articleV1()
			v2.Version = 2
			tc.mutate(&v2)
			err := r.Register(v2)
			var ie *IncompatibleError
			if !errors.As(err, &ie) {
				t.Fatalf("expected IncompatibleError, got %v", err)
			}
			if ie.Reason != tc.reason {
				t.Fatalf("reason = %v, want %v", ie.Reason, tc.reason)
			}
		})
	}
}

func TestValidateUnknownFieldsPreserved(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(articleV1()); err != nil {
		t.Fatal(err)
	}
	sch, _ := r.Latest("article")
	fields := map[string]json.RawMessage{
		"title":     json.RawMessage(`{"en":"hi"}`),
		"body":      json.RawMessage(`{"en":"text"}`),
		"x_future":  json.RawMessage(`{"nested":[1,2,3]}`), // 模型未声明，需原样保留
		"x_tracing": json.RawMessage(`"abc"`),
	}
	if err := sch.Validate(fields); err != nil {
		t.Fatalf("unknown fields must be tolerated: %v", err)
	}
	// 往返：marshal 后未知字段字节不变。
	rt := map[string]json.RawMessage{}
	for k, v := range fields {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var back json.RawMessage
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		rt[k] = back
	}
	if string(rt["x_future"]) != `{"nested":[1,2,3]}` {
		t.Fatalf("unknown field not preserved: %s", rt["x_future"])
	}
}

func TestValidateMissingRequired(t *testing.T) {
	sch := articleV1()
	err := sch.Validate(map[string]json.RawMessage{"title": json.RawMessage(`{"en":"hi"}`)})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "body" {
		t.Fatalf("got %v", err)
	}
}

func TestValidateWrongType(t *testing.T) {
	sch := articleV1()
	err := sch.Validate(map[string]json.RawMessage{
		"title": json.RawMessage(`{"en":"hi"}`),
		"body":  json.RawMessage(`{"en":"x"}`),
		"views": json.RawMessage(`"not-a-number"`),
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "views" {
		t.Fatalf("got %v", err)
	}
}

func TestValidateLocalizedField(t *testing.T) {
	sch := articleV1()
	// 本地化字段必须是 {locale: value} 对象。
	err := sch.Validate(map[string]json.RawMessage{
		"title": json.RawMessage(`"plain-string"`),
		"body":  json.RawMessage(`{"en":"x"}`),
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "title" {
		t.Fatalf("got %v", err)
	}
	// 某个语言的值类型错误也要被检出。
	err = sch.Validate(map[string]json.RawMessage{
		"title": json.RawMessage(`{"en":42}`),
		"body":  json.RawMessage(`{"en":"x"}`),
	})
	if !errors.As(err, &ve) || ve.Field != "title" {
		t.Fatalf("got %v", err)
	}
}
