package store

import (
	"context"
	"errors"

	"github.com/fetburner/rokuban/internal/chapters"
)

var (
	// ErrTrashed は対象がごみ箱にあり、編集できないことを表す。
	ErrTrashed = errors.New("in the trash")
	// ErrStaleVersion は渡された版が現在の状態を指していないことを表す。
	ErrStaleVersion = errors.New("stale version")
	// ErrDetectionPending は必要な検出が終端に達していないことを表す。
	ErrDetectionPending = errors.New("detection is pending")
)

// ChapterSnapshot は録画で選ばれているチャプター層と、CM 検出が未完かを
// 判定するための状態を持つ。
// Version は呼び出し側には不透明で、Replace に渡す以外の意味を持たない。
type ChapterSnapshot struct {
	Owned              bool
	ProgramDurationMs  int64
	CMDetectionEnabled bool
	Detected           bool
	DetectionFailed    bool
	Version            string
	UserSpans          []chapters.Span
	CMRanges           []chapters.Range
}

// DetectionPending は、所有前で検出が終端に達していないかを返す。
// cmDetectEnabled はデプロイ側の CM 検出の有効/無効で、無効なら常に偽になる。
// その構成では検出ジョブが積まれず終端に永久に達しないので、編集を封じると
// 機能ごと使えなくなる。
func (s ChapterSnapshot) DetectionPending(cmDetectEnabled bool) bool {
	return !s.Owned && cmDetectEnabled && s.CMDetectionEnabled && !s.Detected && !s.DetectionFailed
}

// ChapterEdits は録画のユーザー層チャプターを永続化する。
//
// 各メソッドは自身のトランザクションを完了してから返す。Replace と Reset は
// チャプターの状態を読み書きする前に録画の行をロックするので、同じロックを使う
// CM 検出の書き込み、カット版の作り直し要求の記録、カット版の公開と直列化する。
type ChapterEdits interface {
	// Read は有効なチャプター層と不透明な版を返す。
	// 状態と区間は同じスナップショットから読む（一貫したスナップショットを返す）。
	Read(ctx context.Context, recordingID int64) (ChapterSnapshot, error)
	// Replace は expectedVersion が現在の版と一致するときだけ、ユーザー層を原子的に置き換える。
	// requireDetectionComplete は、デプロイが CM 検出を動かしているときに真にする。
	Replace(ctx context.Context, recordingID int64, expectedVersion string, spans []chapters.Span, requireDetectionComplete bool) error
	// Reset はユーザー層を消し、録画を自動層に戻す。
	Reset(ctx context.Context, recordingID int64) error
}
