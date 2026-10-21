// Package catalog は災害復旧用のコアメタデータ JSON の export / rescue を担う
// （docs/storage.md §8、issue #71 M3-9）。
//
// 保護対象はルール・録画履歴・media_assets・原本 TS 計測記録・ドロップ統計と位置・tombstone・
// 手動オーバーライド（と意図の FK 先 program_snapshots）のみ。EPG 射影と
// ジョブキューは再構築可能なので含めない。pg_dump に依存しない。
package catalog

import "github.com/fetburner/rokuban/internal/catalogdoc"

const (
	// Version は catalog JSON のスキーマ版。
	Version = catalogdoc.Version
	// Subdir は media_dir 配下の catalog ディレクトリ名。
	Subdir = catalogdoc.Subdir
	// DefaultKeep は保持する完成世代数の既定値。
	DefaultKeep = catalogdoc.DefaultKeep
	// FilenamePrefix は世代ディレクトリ名の接頭辞。
	FilenamePrefix = catalogdoc.FilenamePrefix
)

// Document は catalog JSON の永続形式を表す。
type Document = catalogdoc.Document

// Recording は recordings の 1 行を表す。
type Recording = catalogdoc.Recording

// MediaAsset は media_assets の 1 行を表す。
type MediaAsset = catalogdoc.MediaAsset

// ProgramSnapshot は program_snapshots の 1 行を表す。
type ProgramSnapshot = catalogdoc.ProgramSnapshot

// ProgramIntent は program_intents の 1 行を表す。
type ProgramIntent = catalogdoc.ProgramIntent
