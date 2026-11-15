package store

import "errors"

var (
	// ErrNotFound は対象が存在しない、または消去済みであることを表す。
	// 対象の文脈は返す側が wrap で足す。
	ErrNotFound = errors.New("not found")
	// ErrConflict は永続化済みのデータが DB の制約と衝突することを表す。
	ErrConflict = errors.New("conflict")
	// ErrEPGStationRuleIDConflict は EPGStation 移行キーの一意性違反を表す。
	// 永続化層は ErrConflict と併せて返す。
	ErrEPGStationRuleIDConflict = errors.New("EPGStation rule ID conflict")
	// ErrInvalidPattern は正規表現が実装の構文として不正であることを表す。
	ErrInvalidPattern = errors.New("invalid pattern")
)
