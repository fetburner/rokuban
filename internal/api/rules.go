package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/fetburner/rokuban/internal/config"
	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/ptr"
	"github.com/fetburner/rokuban/internal/store"
)

// ListRules はルール一覧を返す。
func (h *Server) ListRules(ctx context.Context, _ ListRulesRequestObject) (ListRulesResponseObject, error) {
	rows, err := h.ruleStore.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(rows))
	for _, row := range rows {
		out = append(out, ruleFromStore(row))
	}
	return ListRules200JSONResponse(out), nil
}

// GetRule は指定 ID のルールを返す。
func (h *Server) GetRule(ctx context.Context, req GetRuleRequestObject) (GetRuleResponseObject, error) {
	row, err := h.ruleStore.Get(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return GetRule404JSONResponse{Error: "rule not found"}, nil
		}
		return nil, err
	}
	return GetRule200JSONResponse(ruleFromStore(row)), nil
}

// CreateRule はルールを作成する。
func (h *Server) CreateRule(ctx context.Context, req CreateRuleRequestObject) (CreateRuleResponseObject, error) {
	if req.Body == nil {
		return CreateRule400JSONResponse{Error: "request body is required"}, nil
	}
	// create には保存済みの sites が無いので、レジストリ全件だけが権威になる。
	//
	// 免除（validateRuleSites の savedSites）は「そのルールに保存済みの名前」で切って
	// いるため、既存ルールを下敷きに別のルールを作る経路（検索画面のフォーク。
	// GET で得た sites を preserve してここに載せる）には効かない。レジストリから site が
	// 消えるとフォークだけが 400 になり、sites は条件 UI に無いので UI 内の復旧手段も
	// 無い（TestRuleSites_RegistryDriftRejectsFork が測っている）。免除の切り方を
	// 「クライアントが GET で読んで載せ直した名前」に寄せるか web 側で明示的に外させるかは
	// API の形 / UI の形を決める判断なので、実装せず issue に提起している
	// （docs/schema/rules.md の rule_sites 節）。
	//nolint:nilerr // 検証エラーは HTTP 400 のレスポンスとして返す。
	if err := h.validateRuleInput(*req.Body, nil); err != nil {
		return CreateRule400JSONResponse{Error: err.Error()}, nil
	}
	if msg, err := h.ruleRegexError(ctx, *req.Body); err != nil {
		return nil, err
	} else if msg != "" {
		return CreateRule400JSONResponse{Error: msg}, nil
	}

	row, err := h.ruleStore.Create(ctx, ruleInputFromAPI(*req.Body))
	if err != nil {
		return nil, err
	}
	h.enqueueRulerPassHintsForRuleSites(ctx, row.Sites)
	return CreateRule201JSONResponse(ruleFromStore(row)), nil
}

// UpdateRule はルールを上書き更新する（子テーブルは全置換）。
func (h *Server) UpdateRule(ctx context.Context, req UpdateRuleRequestObject) (UpdateRuleResponseObject, error) {
	if req.Body == nil {
		return UpdateRule400JSONResponse{Error: "request body is required"}, nil
	}
	// 保存済みの sites はレジストリ照合を免除する（validateRuleSites 参照）。tx の外で
	// 読むが、免除集合は「受け付ける名前」を広げるだけで、しかも広げる先はこのルールが
	// 直前まで持っていた名前に限られる。並行 PATCH が同じルールの sites を入れ替えた場合の
	// 結果は 2 つの PATCH が逆順に届いたのと同じ（子テーブルは全置換で後勝ち）なので、
	// tx 内に入れても変わらない。
	//
	// **検証は GetRule より前**（既存の順序だが、免除を入れたことで意味が付いた）。
	// 存在しないルールでは savedRuleSites が空集合になるので免除が何も広げず、
	// 「存在しない id + 未知 site」は 404 ではなく 400 unknown site になる
	// （TestUpdateRule_UnknownSiteBeatsNotFound。妥当な入力なら 404 に落ちる）。
	// 404 を先に返したいなら、免除集合の読みも GetRule の後に動かすこと。
	var savedSites map[string]struct{}
	if req.Body.Sites != nil {
		var err error
		var sites []string
		sites, err = h.ruleStore.Sites(ctx, req.Id)
		if err != nil {
			return nil, err
		}
		savedSites = siteSet(sites)
	}
	//nolint:nilerr // 検証エラーは HTTP 400 のレスポンスとして返す。
	if err := h.validateRuleInput(*req.Body, savedSites); err != nil {
		return UpdateRule400JSONResponse{Error: err.Error()}, nil
	}
	if msg, err := h.ruleRegexError(ctx, *req.Body); err != nil {
		return nil, err
	} else if msg != "" {
		return UpdateRule400JSONResponse{Error: msg}, nil
	}

	row, err := h.ruleStore.Update(ctx, req.Id, ruleInputFromAPI(*req.Body))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return UpdateRule404JSONResponse{Error: "rule not found"}, nil
		}
		return nil, err
	}
	h.enqueueRulerPassHintsForRuleSites(ctx, row.Sites)
	return UpdateRule200JSONResponse(ruleFromStore(row)), nil
}

// DeleteRule はルールを削除する。overrides なし予約は削除、ありは detached 化。
func (h *Server) DeleteRule(ctx context.Context, req DeleteRuleRequestObject) (DeleteRuleResponseObject, error) {
	deleted, err := h.ruleStore.Delete(ctx, req.Id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeleteRule404JSONResponse{Error: "rule not found"}, nil
		}
		return nil, err
	}
	h.enqueueRulerPassHintsForRuleSites(ctx, deleted.Sites)

	return DeleteRule200JSONResponse{
		Id:                   req.Id,
		DeletedReservations:  int(deleted.DeletedReservations),
		DetachedReservations: int(deleted.DetachedReservations),
	}, nil
}

// validateEncodeProfiles は encodeProfiles の各名前が config 定義に存在すること、
// および live 無効時の cut 選択規則を
// 検査する。ルール保存・予約 overrides・事後追加 API の 3 経路がここを通る
// （4 経路目は ingest の凍結で、同じ config.ValidateCutSelection を直接呼ぶ）。
//
// h.encodeProfiles が nil のとき（RouterConfig.EncodeProfileNames 未設定 = テストの
// 部分構成）は名前検証をスキップする。空 map（len=0 だが non-nil）は「プロファイルが
// 1 つも無い」とみなし、どんな名前も未知として弾く。
//
// cut の規則も同じ規約で、h.cutProfiles が nil なら検査しない。cut プロファイルが
// 1 つも定義されていない構成では config.ValidateCutSelection 自体が何も主張しない。
// h.capabilities.Live は config.live.enabled そのもの。
func (h *Server) validateEncodeProfiles(names []string) error {
	if err := h.validateEncodeProfileNames(names); err != nil {
		return err
	}
	return h.validateCutSelection(names)
}

// validateCutSelection は API のルール・override・事後追加が共有する cut 選択判定。
func (h *Server) validateCutSelection(names []string) error {
	if h.cutProfiles == nil {
		return nil
	}
	return config.ValidateCutSelection(names, h.cutProfiles, h.capabilities.Live)
}

// validateEncodeProfileNames は名前だけの検査（空名・未知名）。cut の選択規則は見ない。
// 「既存 ∪ 追加分」に規則を当てる事後追加と、cut 専用の作り直しがこちらを直接使う。
func (h *Server) validateEncodeProfileNames(names []string) error {
	for _, name := range names {
		if name == "" {
			return errors.New("encodeProfiles must not contain empty names")
		}
		if h.encodeProfiles != nil {
			if _, ok := h.encodeProfiles[name]; !ok {
				return fmt.Errorf("unknown encode profile %q", name)
			}
		}
	}
	return nil
}

// validateRuleSites は sites の各要素が site レジストリ（config.mirakcs、
// h.sites）に存在することを検査する。api は不変条件 1 によりどの site にも束縛されないので、
// 権威は h.knownSite が参照するレジストリ全件であり、mirakc への問い合わせも FS 依存もしない。
// 空文字列も未知の site 名と同じ扱いで 400 にする --- 「絞り込みたい」意図を持つ要素が黙って
// 「全サイト」に反転するのを防ぐ（validateEncodeProfiles が空名を拒否する流儀に揃える）。
// 全サイトを意図するなら sites 全体を省略・空配列にする（FK を張らない代わりの書き込み時
// 照合であり、CHECK ではなく 400 で拒否する）。
//
// savedSites はそのルールに保存済みの rule_sites（create では nil）。**保存済みの名前は
// レジストリに無くても通す。** 照合が狙うタイポは定義上「保存済みに無い新しい名前」なので
// 検出力は落ちず、レジストリから site が消えたときに保存済みの値をそのまま載せ直す
// read-modify-write な更新（優先度や名前だけ直したい編集を含む）が全部 400 になるのを防ぐ
// --- 消えた site の掃除はこの照合の仕事ではない。両方向は
// TestRuleSites_RegistryDriftAllowsRoundTripUpdate が押さえている。
//
// 免除の境界は**ルールの同一性**であり「クライアントが GET で読んで載せ直した名前」では
// ない。そのため savedSites が nil な create（フォークを含む）は同じ read-modify-write でも
// 400 になる（CreateRule のコメントと TestRuleSites_RegistryDriftRejectsFork）。
func (h *Server) validateRuleSites(sitesIn []string, savedSites map[string]struct{}) error {
	for _, site := range sitesIn {
		if site == "" {
			return errors.New("sites must not contain empty names")
		}
		if h.knownSite(site) {
			continue
		}
		if _, saved := savedSites[site]; saved {
			continue
		}
		return fmt.Errorf("unknown site %q", site)
	}
	return nil
}

// siteSet は保存済み site の一覧を membership 判定用の集合へ変換する。
func siteSet(sites []string) map[string]struct{} {
	set := make(map[string]struct{}, len(sites))
	for _, site := range sites {
		set[site] = struct{}{}
	}
	return set
}

// validateRuleInput はルール create/update の入力を検査する。
// encodeProfiles の存在検証は h.encodeProfiles（config から注入）を使う。
// 集合が nil のときは名前検証をスキップする（テストの部分構成）。
// savedSites は sites のレジストリ照合を免除する保存済みの site 名（create では nil。
// validateRuleSites 参照）。
func (h *Server) validateRuleInput(in RuleInput, savedSites map[string]struct{}) error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	if in.KeepOriginal != nil && *in.KeepOriginal == RuleInputKeepOriginalUntilEncoded {
		if in.EncodeProfiles == nil || len(*in.EncodeProfiles) == 0 {
			return errors.New("encodeProfiles is required when keepOriginal is until_encoded")
		}
	}
	if in.EncodeProfiles != nil {
		if err := h.validateEncodeProfiles(*in.EncodeProfiles); err != nil {
			return err
		}
	}
	if in.DedupeEnabled != nil && *in.DedupeEnabled {
		if in.DedupeThreshold == nil {
			return errors.New("dedupeThreshold is required when dedupeEnabled is true")
		}
	}
	// dedupeThreshold は pg_trgm の similarity() と比較される（internal/store/pgstore/rulerpass.go の
	// similarity(rec.title, c.title) >= c.dedupe_threshold）。similarity() の値域は (0, 1] の
	// 半開区間として扱う: 0 を許すと "similarity() >= 0" が恒真になり、そのルールに finished の
	// 録画が 1 本でもあれば以降マッチする全番組に base.skip が立って録画が黙って止まる
	// （サーキットブレーカーは削除しか守らないのでこの経路は何にも止められない）。
	// 1 を超えると恒偽になり重複排除が黙って無効化される。したがって 0 は許さず、
	// (0, 1] の範囲だけを許可する。
	if in.DedupeThreshold != nil {
		if v := *in.DedupeThreshold; v <= 0 || v > 1 {
			return fmt.Errorf("dedupeThreshold must be > 0 and <= 1 (similarity() ranges over [0,1]; 0 would match every program and silently stop recording), got %v", v)
		}
	}
	// dedupeWindowSeconds は interval に変換され、internal/store/pgstore/rulerpass.go の
	// "rec.program_start_at >= $3 - c.dedupe_window"（$3 は ruler が渡す評価時刻）の右辺に使われる。
	//
	// **0 も弾く。** 0 は「時間窓なし」ではない —— 窓なしは NULL（列を省略する）であり、
	// 0 を入れると条件が "program_start_at >= 評価時刻" になって、比較対象が必ず過去の
	// 放送である以上つねに偽になる。重複排除が黙って無効化されるという、負値とまったく
	// 同じ失敗モードである（dedupeThreshold の 0 を弾くのと対称）。窓を設けたくない
	// なら値を送らない。
	if in.DedupeWindowSeconds != nil && *in.DedupeWindowSeconds <= 0 {
		return fmt.Errorf("dedupeWindowSeconds must be > 0 (0 is not \"no window\" — omit the field for that; "+
			"0 makes the window condition always false and silently disables dedupe), got %d", *in.DedupeWindowSeconds)
	}
	if in.Sites != nil {
		if err := h.validateRuleSites(*in.Sites, savedSites); err != nil {
			return err
		}
	}
	if in.FilenameTemplate != nil && *in.FilenameTemplate != "" {
		if err := contentpath.Validate(*in.FilenameTemplate); err != nil {
			return fmt.Errorf("invalid filenameTemplate %q (Go text/template; see docs/recording.md §3.2): %w",
				*in.FilenameTemplate, err)
		}
	}
	return nil
}

// ruleRegexError は入力の正規表現を検査し、不正なら 400 の本文を返す。
// 検査自体の失敗は err で返す（500）。
func (h *Server) ruleRegexError(ctx context.Context, in RuleInput) (string, error) {
	if in.TextMatches == nil {
		return "", nil
	}
	for _, m := range *in.TextMatches {
		if m.Mode != Regex {
			continue
		}
		if msg, err := regexPatternError(ctx, h, m.Value); msg != "" || err != nil {
			return msg, err
		}
	}
	return "", nil
}

func ruleInputFromAPI(in RuleInput) store.RuleInput {
	out := store.RuleInput{
		Name: strings.TrimSpace(in.Name), Description: in.Description, Enabled: in.Enabled,
		IsFree: in.IsFree, DurationMinMs: in.DurationMinMs, DurationMaxMs: in.DurationMaxMs,
		PeriodStartAt: in.PeriodStartAt, PeriodEndAt: in.PeriodEndAt,
		DedupeEnabled: in.DedupeEnabled, DedupeThreshold: in.DedupeThreshold,
		DedupeWindowSeconds: in.DedupeWindowSeconds, FilenameTemplate: in.FilenameTemplate,
		Metadata: in.Metadata,
	}
	if in.Priority != nil {
		priority := int32(*in.Priority)
		out.Priority = &priority
	}
	if in.KeepOriginal != nil {
		value := string(*in.KeepOriginal)
		out.KeepOriginal = &value
	}
	if in.EncodeProfiles != nil {
		values := append([]string(nil), (*in.EncodeProfiles)...)
		out.EncodeProfiles = &values
	}
	if in.TextMatches != nil {
		out.TextMatches = make([]store.RuleTextMatch, 0, len(*in.TextMatches))
		for _, match := range *in.TextMatches {
			out.TextMatches = append(out.TextMatches, store.RuleTextMatch{
				Target: string(match.Target), Mode: string(match.Mode), Value: match.Value,
				CaseSensitive: ptr.Deref(match.CaseSensitive), Negate: ptr.Deref(match.Negate),
			})
		}
	}
	if in.Services != nil {
		out.Services = make([]store.RuleService, 0, len(*in.Services))
		for _, service := range *in.Services {
			out.Services = append(out.Services, store.RuleService{
				NetworkID: int32(service.NetworkId), ServiceID: int32(service.ServiceId),
			})
		}
	}
	if in.ChannelTypes != nil {
		out.ChannelTypes = make([]string, 0, len(*in.ChannelTypes))
		for _, channelType := range *in.ChannelTypes {
			out.ChannelTypes = append(out.ChannelTypes, string(channelType))
		}
	}
	if in.Genres != nil {
		out.Genres = make([]int16, 0, len(*in.Genres))
		for _, genre := range *in.Genres {
			out.Genres = append(out.Genres, int16(genre))
		}
	}
	if in.Times != nil {
		out.Times = make([]store.RuleTimeWindow, 0, len(*in.Times))
		for _, tw := range *in.Times {
			out.Times = append(out.Times, store.RuleTimeWindow{
				Weekdays: int32(tw.Weekdays), StartSec: int32(tw.StartSec), EndSec: int32(tw.EndSec),
			})
		}
	}
	if in.Sites != nil {
		out.Sites = append([]string(nil), (*in.Sites)...)
	}
	return out
}

func ruleFromStore(rule store.Rule) Rule {
	description := rule.Description
	filenameTemplate := rule.FilenameTemplate
	dedupeEnabled := rule.DedupeEnabled
	out := Rule{
		Id: rule.ID, Name: rule.Name, Description: &description, Enabled: rule.Enabled,
		Priority: int(rule.Priority), KeepOriginal: RuleKeepOriginal(rule.KeepOriginal),
		FilenameTemplate: &filenameTemplate, CreatedAt: rule.CreatedAt, UpdatedAt: rule.UpdatedAt,
		IsFree: rule.IsFree, DurationMinMs: rule.DurationMinMs, DurationMaxMs: rule.DurationMaxMs,
		PeriodStartAt: rule.PeriodStartAt, PeriodEndAt: rule.PeriodEndAt,
		DedupeEnabled: &dedupeEnabled, DedupeThreshold: rule.DedupeThreshold,
		DedupeWindowSeconds: rule.DedupeWindowSeconds,
	}
	if len(rule.EncodeProfiles) > 0 {
		values := slices.Clone(rule.EncodeProfiles)
		out.EncodeProfiles = &values
	}
	if len(rule.Metadata) > 0 {
		metadata := rule.Metadata
		out.Metadata = &metadata
	}
	if len(rule.TextMatches) > 0 {
		matches := make([]RuleTextMatch, 0, len(rule.TextMatches))
		for _, match := range rule.TextMatches {
			caseSensitive, negate := match.CaseSensitive, match.Negate
			matches = append(matches, RuleTextMatch{
				Target: RuleTextMatchTarget(match.Target), Mode: RuleTextMatchMode(match.Mode),
				Value: match.Value, CaseSensitive: &caseSensitive, Negate: &negate,
			})
		}
		out.TextMatches = &matches
	}
	if len(rule.Services) > 0 {
		services := make([]RuleService, 0, len(rule.Services))
		for _, service := range rule.Services {
			services = append(services, RuleService{NetworkId: int(service.NetworkID), ServiceId: int(service.ServiceID)})
		}
		out.Services = &services
	}
	if len(rule.ChannelTypes) > 0 {
		channelTypes := make([]RuleChannelTypes, 0, len(rule.ChannelTypes))
		for _, channelType := range rule.ChannelTypes {
			channelTypes = append(channelTypes, RuleChannelTypes(channelType))
		}
		out.ChannelTypes = &channelTypes
	}
	if len(rule.Genres) > 0 {
		genres := make([]int, 0, len(rule.Genres))
		for _, genre := range rule.Genres {
			genres = append(genres, int(genre))
		}
		out.Genres = &genres
	}
	if len(rule.Times) > 0 {
		times := make([]RuleTimeWindow, 0, len(rule.Times))
		for _, tw := range rule.Times {
			times = append(times, RuleTimeWindow{
				Weekdays: int(tw.Weekdays), StartSec: int(tw.StartSec), EndSec: int(tw.EndSec),
			})
		}
		out.Times = &times
	}
	if len(rule.Sites) > 0 {
		sites := slices.Clone(rule.Sites)
		out.Sites = &sites
	}
	return out
}
