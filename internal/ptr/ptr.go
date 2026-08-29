// Package ptr はポインタの nil を安全なゼロ値に落とす共通ヘルパーを持つ。
package ptr

// Deref は p が nil ならゼロ値を、そうでなければ *p を返す。
func Deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
