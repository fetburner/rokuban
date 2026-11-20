// Package storagename は置き場（storage store）の名前の文法を置く。
// 名前は小文字 ASCII・数字・ハイフンだけで、ファイル名や DB の値として安全に使える形に限る。
package storagename

// ValidStoreName は置き場名が小文字 ASCII・数字・ハイフンだけで構成されるかを返す。
func ValidStoreName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}
