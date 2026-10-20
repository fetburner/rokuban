// Package catalog manages catalog files and rescue orchestration.
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

// LabelRule は label_rules の 1 行を表す。
type LabelRule = catalogdoc.LabelRule

// Rule は rules 本体と子テーブルをまとめた 1 ルール分を表す。
type Rule = catalogdoc.Rule

// RuleTextMatch は rule_text_matches の 1 行を表す。
type RuleTextMatch = catalogdoc.RuleTextMatch

// RuleService は rule_services の 1 行を表す。
type RuleService = catalogdoc.RuleService

// RuleTime は rule_times の 1 行を表す。
type RuleTime = catalogdoc.RuleTime

// Recording は recordings の 1 行を表す。
type Recording = catalogdoc.Recording

// RecordingEncodePolicy は recording_encode_policy の 1 行を表す。
type RecordingEncodePolicy = catalogdoc.RecordingEncodePolicy

// RecordingPurgeRequest は recording_purge_requests の 1 行を表す。
type RecordingPurgeRequest = catalogdoc.RecordingPurgeRequest

// RecordingWatched は recording_watched の 1 行を表す。
type RecordingWatched = catalogdoc.RecordingWatched

// RecordingChapterOwnership は recording_chapter_ownership の 1 行を表す。
type RecordingChapterOwnership = catalogdoc.RecordingChapterOwnership

// RecordingChapterSpan は recording_chapter_spans の 1 行を表す。
type RecordingChapterSpan = catalogdoc.RecordingChapterSpan

// MediaAsset は media_assets の 1 行を表す。
type MediaAsset = catalogdoc.MediaAsset

// DropStat は drop_stats の 1 行を表す。
type DropStat = catalogdoc.DropStat

// DropPosition は drop_positions の 1 行を表す。
type DropPosition = catalogdoc.DropPosition

// MediaAssetTSScan は media_asset_ts_scans の 1 行を表す。
type MediaAssetTSScan = catalogdoc.MediaAssetTSScan

// ProgramSnapshot は program_snapshots の 1 行を表す。
type ProgramSnapshot = catalogdoc.ProgramSnapshot

// ProgramIntent は program_intents の 1 行を表す。
type ProgramIntent = catalogdoc.ProgramIntent

// ProgramOverride は program_overrides の 1 行を表す。
type ProgramOverride = catalogdoc.ProgramOverride
