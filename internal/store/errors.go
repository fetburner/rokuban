package store

import "errors"

var (
	// ErrNotFound は対象が存在しない、または消去済みであることを表す。
	// 対象の文脈は返す側が wrap で足す。
	ErrNotFound = errors.New("not found")
	// ErrConflict は永続化済みのデータが DB の制約と衝突することを表す。
	ErrConflict = errors.New("conflict")
	// ErrInvalidPattern は正規表現が実装の構文として不正であることを表す。
	ErrInvalidPattern = errors.New("invalid pattern")
	// ErrEncodeProfileSelectionInvalid はロック後の desired に対する呼び出し側の検証が拒否したことを表す。
	ErrEncodeProfileSelectionInvalid = errors.New("invalid encode profile selection")
	// ErrOriginalUnavailable は有効な原本がなく、エンコード操作を受け付けられないことを表す。
	ErrOriginalUnavailable = errors.New("active original required")
	// ErrEncodedAssetNotFound は削除対象の有効なエンコード版がないことを表す。
	ErrEncodedAssetNotFound = errors.New("encoded asset not found")
	// ErrEncodeProfilesRequired は until_encoded に必要な desired が空であることを表す。
	ErrEncodeProfilesRequired = errors.New("desired encode profiles required")
	// ErrChaptersNotOwned はユーザーが確認したチャプターがなく、カット版を作れないことを表す。
	ErrChaptersNotOwned = errors.New("confirmed chapters required")
	// ErrKeepRangesEmpty はカット版に使える本編区間がないことを表す。
	ErrKeepRangesEmpty = errors.New("no keep ranges")
	// ErrProfileNotDesired は要求された作り直し対象が desired に含まれないことを表す。
	ErrProfileNotDesired = errors.New("profile is not desired")
	// ErrLastViewableCopy は削除要求の適用後に再生できるコピーが残らないことを表す。
	ErrLastViewableCopy = errors.New("last viewable copy")
)
