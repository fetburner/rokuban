package store

import "context"

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
	// 方針行をロックした後の desired を validate に渡し、その検証と追加を直列化する。
	AppendProfiles(ctx context.Context, recordingID int64, names []string, validate func(desired []string) error) error

	// RemoveEncodedAsset は録画の行から方針の行の順に変更を直列化し、版を 1 本外す。
	// validate は profile がロック後の desired に含まれる場合だけ呼ぶ。
	RemoveEncodedAsset(ctx context.Context, recordingID int64, profile string, validate func(desired []string) error) error

	// RequestCutRebuild は現在の確認済みチャプターから keep を導出して作り直し要求を記録する。
	// チャプター編集と同じ録画行の直列化の下で状態を読む。
	RequestCutRebuild(ctx context.Context, recordingID int64, profile string) error

	// SetPolicy は指定された保持・CM 検出方針を 1 つのトランザクションで更新する。
	SetPolicy(ctx context.Context, recordingID int64, update EncodePolicyUpdate) error
}
