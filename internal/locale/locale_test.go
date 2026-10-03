package locale

import "testing"

var chain = Chain{"de-AT", "de", "en"}

func TestResolveExplicitValue(t *testing.T) {
	v := Values{"de-AT": "Servus", "en": "Hello"}
	r := v.Resolve(chain)
	if !r.Found || !r.Explicit || r.Value != "Servus" || r.Locale != "de-AT" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveFallbackOrder(t *testing.T) {
	v := Values{"de": "Hallo", "en": "Hello"}
	r := v.Resolve(chain)
	if !r.Found || r.Explicit || r.Value != "Hallo" || r.Locale != "de" {
		t.Fatalf("got %+v", r)
	}
	// 跳过缺失层级，落到 en。
	v2 := Values{"en": "Hello"}
	r2 := v2.Resolve(chain)
	if !r2.Found || r2.Explicit || r2.Value != "Hello" || r2.Locale != "en" {
		t.Fatalf("got %+v", r2)
	}
}

func TestExplicitEmptyBeatsFallback(t *testing.T) {
	// 显式填写的空串必须区别于缺失：不触发回退。
	v := Values{"de-AT": "", "en": "Hello"}
	r := v.Resolve(chain)
	if !r.Found || !r.Explicit || r.Value != "" || r.Locale != "de-AT" {
		t.Fatalf("explicit empty must win over fallback, got %+v", r)
	}
}

func TestNotFound(t *testing.T) {
	r := Values{}.Resolve(chain)
	if r.Found {
		t.Fatalf("expected not found, got %+v", r)
	}
}
