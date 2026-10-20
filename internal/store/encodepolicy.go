package store

import (
	"context"
	"errors"
)

var (
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

// EncodePolicyUpdate は録画単位で更新する方針項目を表す。
// nil の項目は変更しない。
type EncodePolicyUpdate struct {
	KeepOriginal *string
	CMDetect     *bool
}

// EncodePolicy はエンコード方針とユーザー起点の作り直し要求を永続化する。
// 各メソッドは自身のトランザクションを完了してから返す。
type EncodePolicy interface {
	// AppendProfiles は active な原本を持つ録画の desired に名前を追加し、失敗試行の予算を戻す。
	// 同じ録画の方針の行で RemoveEncodedAsset・他の AppendProfiles と直列化する。
	// desired を読む側（RequestCutRebuild の desired 検査、エンコードの公開直前の desired 判定）は、
	// AppendProfiles の commit 後の desired を見る。
	// validate には方針の行を取った後の、追加を反映した desired を渡して 1 回呼ぶ。
	// validate が返したエラーは包まずそのまま返す。
	AppendProfiles(ctx context.Context, recordingID int64, names []string, validate func(desired []string) error) error

	// RemoveEncodedAsset は録画のエンコード版を 1 本外す。
	// 録画の行を取る他の書き手すべて（同じ録画の別プロファイルの削除・RequestCutRebuild を含む）と直列化し、
	// 続いて方針の行で AppendProfiles と直列化する。順序は録画の行、方針の行の順である。
	// validate は profile がロック後の desired に含まれる場合だけ、profile を除いた desired で呼ぶ。
	// validate が返したエラーは包まずそのまま返す。
	RemoveEncodedAsset(ctx context.Context, recordingID int64, profile string, validate func(desired []string) error) error

	// RequestCutRebuild は現在の確認済みチャプターから keep を導出して作り直し要求を記録する。
	// 録画の行を取る他の書き手すべてと直列化し、keep はチャプター編集の commit 後の確認済みチャプターから導く。
	// desired の検査は、AppendProfiles・RemoveEncodedAsset の commit 後の desired を見る。
	// 順序は録画の行、方針の行の順である。
	RequestCutRebuild(ctx context.Context, recordingID int64, profile string) error

	// SetPolicy は指定された保持・CM 検出方針を 1 つのトランザクションで更新する。
	// 先にロックを取らず、読んだ値で判断しない。
	// 適用条件（until_encoded には空でない desired、CM 検出の有効化には active な原本）は
	// 書き込む瞬間の行に対して判定するので、並行する追加・削除の commit 後の desired に対して効く。
	SetPolicy(ctx context.Context, recordingID int64, update EncodePolicyUpdate) error
}
