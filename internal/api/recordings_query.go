package api

import (
	"fmt"

	"github.com/fetburner/rokuban/internal/store"
)

// defaultRecordingsLimit / maxRecordingsLimit は GET /api/recordings の
// limit の既定値・上限（openapi.yaml のパラメータ定義と一致させる）。
const (
	defaultRecordingsLimit = 50
	maxRecordingsLimit     = 200
)

// genreLv1Min / genreLv1Max は genre_lv1 のドメイン（rule_genres.genre_lv1 の
// CHECK と同じ、recordings.genre_lv1 を作る生成列関数 genre_lv1_of の
// 範囲チェック参照）。範囲外の genre クエリパラメータは黙って 0 件に落とさず 400 にする。
const (
	genreLv1Min = 0
	genreLv1Max = 15
)

// recordingsFilterFromParams は ListRecordingsParams（OpenAPI の生成型）を
// store の型付きフィルタへ変換する。不正な入力はエラーメッセージを返す。
func recordingsFilterFromParams(p ListRecordingsParams) (store.RecordingFilter, string) {
	f := store.RecordingFilter{
		Trash:    p.Trash != nil && *p.Trash,
		SortDesc: true,
		Limit:    defaultRecordingsLimit,
		RuleID:   p.RuleId,
		SeriesOf: p.SeriesOf,
		From:     p.From,
		To:       p.To,
		Before:   p.Before,
		BeforeID: p.BeforeId,
	}
	if p.Order != nil {
		if !p.Order.Valid() {
			return store.RecordingFilter{}, fmt.Sprintf("invalid order %q (want asc or desc)", *p.Order)
		}
		f.SortDesc = *p.Order != Asc
	}
	if p.Q != nil {
		f.Q = *p.Q
	}
	if p.QTarget != nil {
		if !p.QTarget.Valid() {
			return store.RecordingFilter{}, fmt.Sprintf("invalid qTarget %q (want title or titleDescription)", *p.QTarget)
		}
		f.QTarget = string(*p.QTarget)
	}
	if p.Genre != nil {
		for _, g := range *p.Genre {
			if g < genreLv1Min || g > genreLv1Max {
				return store.RecordingFilter{}, fmt.Sprintf("genre must be between %d and %d, got %d", genreLv1Min, genreLv1Max, g)
			}
		}
		f.Genres = int16SliceFromInts(*p.Genre)
	}
	if p.ChannelType != nil {
		for _, ct := range *p.ChannelType {
			if !ct.Valid() {
				return store.RecordingFilter{}, fmt.Sprintf("invalid channelType %q (want GR, BS, CS or SKY)", ct)
			}
		}
		f.ChannelTypes = channelTypeStrings(*p.ChannelType)
	}
	if p.Site != nil {
		f.Sites = *p.Site
	}
	if p.Service != nil {
		networkIDs, serviceIDs, msg := splitServiceIDs(*p.Service)
		if msg != "" {
			return store.RecordingFilter{}, msg
		}
		for i := range networkIDs {
			f.Services = append(f.Services, store.RecordingServiceRef{
				NetworkID: networkIDs[i], ServiceID: serviceIDs[i],
			})
		}
	}
	if p.Status != nil {
		if !p.Status.Valid() {
			return store.RecordingFilter{}, fmt.Sprintf("invalid status %q", *p.Status)
		}
		f.Status = string(*p.Status)
	}
	if p.EncodeState != nil && !p.EncodeState.Valid() {
		return store.RecordingFilter{}, fmt.Sprintf("invalid encodeState %q (want queued, running or awaiting_review)", *p.EncodeState)
	}
	if p.Source != nil {
		if !p.Source.Valid() {
			return store.RecordingFilter{}, fmt.Sprintf("invalid source %q (want rule, manual or unattributed)", *p.Source)
		}
		f.Source = string(*p.Source)
	}
	if p.Limit != nil {
		f.Limit = *p.Limit
	}

	if (f.Before == nil) != (f.BeforeID == nil) {
		return store.RecordingFilter{}, "before and beforeId must be given together"
	}
	if f.Limit < 1 || f.Limit > maxRecordingsLimit {
		return store.RecordingFilter{}, fmt.Sprintf("limit must be between 1 and %d", maxRecordingsLimit)
	}
	return f, ""
}

// int16SliceFromInts は openapi の genre（[]int）を genre_lv1 の smallint[] 引数に
// 変換する。呼び出し側が範囲を検証済みであることが前提。
func int16SliceFromInts(vs []int) []int16 {
	out := make([]int16, len(vs))
	for i, v := range vs {
		out[i] = int16(v)
	}
	return out
}

// channelTypeStrings は ListRecordingsParamsChannelType の列を素の文字列列に変換する。
func channelTypeStrings(vs []ListRecordingsParamsChannelType) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}
