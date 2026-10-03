// Package locale 提供多语言值的确定性回退解析。
//
// 语义要点：
//   - 某语言键存在即视为“显式赋值”，即使值是空字符串；
//     显式空值不会触发回退，调用方拿到的 Explicit=true、Value=""。
//   - 键缺失才按回退链逐级解析，结果标记 Explicit=false 并记录来源语言。
package locale

// Values 是某一字段在各语言下的显式取值。
// 键存在即视为显式赋值（即使为空字符串），键缺失才会触发回退。
type Values map[string]string

// Chain 是按优先级排列的回退顺序，例如 ["de-AT", "de", "en"]。
// 链首即请求语言。
type Chain []string

// Result 是一次回退解析的结果。
type Result struct {
	Value    string // 解析得到的值
	Locale   string // 值实际来源的语言
	Explicit bool   // true 表示在请求语言上显式填写（可能为空串）；false 表示来自回退
	Found    bool   // 整条链都没有任何值时为 false
}

// Resolve 按链顺序解析：请求语言（链首）显式存在的值（含空串）直接返回；
// 否则逐级回退，并标记为回退结果。
func (v Values) Resolve(chain Chain) Result {
	for i, loc := range chain {
		val, ok := v[loc]
		if !ok {
			continue
		}
		return Result{
			Value:    val,
			Locale:   loc,
			Explicit: i == 0,
			Found:    true,
		}
	}
	return Result{Found: false}
}
