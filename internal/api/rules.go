package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fetburner/rokuban/internal/contentpath"
	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/worker"
)

// ListRules はルール一覧を返す。
func (h *Server) ListRules(ctx context.Context, _ ListRulesRequestObject) (ListRulesResponseObject, error) {
	q := sqlcgen.New(h.pool)
	rows, err := q.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(rows))
	for _, row := range rows {
		full, err := loadRule(ctx, q, row)
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return ListRules200JSONResponse(out), nil
}

// GetRule は指定 ID のルールを返す。
func (h *Server) GetRule(ctx context.Context, req GetRuleRequestObject) (GetRuleResponseObject, error) {
	q := sqlcgen.New(h.pool)
	row, err := q.GetRule(ctx, req.Id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return GetRule404JSONResponse{Error: "rule not found"}, nil
		}
		return nil, err
	}
	full, err := loadRule(ctx, q, row)
	if err != nil {
		return nil, err
	}
	return GetRule200JSONResponse(full), nil
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
	if err := h.validateRuleInput(ctx, *req.Body, nil); err != nil {
		return CreateRule400JSONResponse{Error: err.Error()}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	row, err := insertRule(ctx, q, *req.Body)
	if err != nil {
		return nil, err
	}
	if err := replaceRuleChildren(ctx, q, row.ID, *req.Body); err != nil {
		return nil, err
	}
	sites, err := ruleTargetSites(ctx, q, row.ID)
	if err != nil {
		return nil, err
	}
	if err := h.insertRulerPassHintsForRuleSites(ctx, tx, sites); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	full, err := loadRule(ctx, sqlcgen.New(h.pool), row)
	if err != nil {
		return nil, err
	}
	return CreateRule201JSONResponse(full), nil
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
		savedSites, err = savedRuleSites(ctx, sqlcgen.New(h.pool), req.Id)
		if err != nil {
			return nil, err
		}
	}
	if err := h.validateRuleInput(ctx, *req.Body, savedSites); err != nil {
		return UpdateRule400JSONResponse{Error: err.Error()}, nil
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	if _, err := q.GetRule(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UpdateRule404JSONResponse{Error: "rule not found"}, nil
		}
		return nil, err
	}

	params, err := ruleUpdateParams(req.Id, *req.Body)
	if err != nil {
		return UpdateRule400JSONResponse{Error: err.Error()}, nil
	}
	row, err := q.UpdateRule(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := replaceRuleChildren(ctx, q, row.ID, *req.Body); err != nil {
		return nil, err
	}
	sites, err := ruleTargetSites(ctx, q, row.ID)
	if err != nil {
		return nil, err
	}
	if err := h.insertRulerPassHintsForRuleSites(ctx, tx, sites); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	full, err := loadRule(ctx, sqlcgen.New(h.pool), row)
	if err != nil {
		return nil, err
	}
	return UpdateRule200JSONResponse(full), nil
}

// DeleteRule はルールを削除する。overrides なし予約は削除、ありは detached 化。
func (h *Server) DeleteRule(ctx context.Context, req DeleteRuleRequestObject) (DeleteRuleResponseObject, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	if _, err := q.GetRule(ctx, req.Id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeleteRule404JSONResponse{Error: "rule not found"}, nil
		}
		return nil, err
	}

	// 対象サイトも先に読む（rule_sites は rules への ON DELETE CASCADE。
	// DeleteRule の後では読めない）。
	sites, err := ruleTargetSites(ctx, q, req.Id)
	if err != nil {
		return nil, err
	}

	// 内訳は先に数える（削除後には数えられない）。
	// record 意図または上書きのある予約は残り、FK の ON DELETE SET NULL で
	// rule_id が外れて実質 manual として動く。意図自体は program_intents に
	// あるので消えない。
	id := req.Id
	detached, err := q.CountReservationsByRuleWithIntent(ctx, &id)
	if err != nil {
		return nil, fmt.Errorf("counting reservations with intent: %w", err)
	}
	deleted, err := q.DeleteReservationsByRuleWithoutIntent(ctx, &id)
	if err != nil {
		return nil, fmt.Errorf("deleting pure rule reservations: %w", err)
	}
	n, err := q.DeleteRule(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return DeleteRule404JSONResponse{Error: "rule not found"}, nil
	}
	if err := h.insertRulerPassHintsForRuleSites(ctx, tx, sites); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return DeleteRule200JSONResponse{
		Id:                   req.Id,
		DeletedReservations:  int(deleted),
		DetachedReservations: int(detached),
	}, nil
}

// insertRulerPassHint は指定した site の RulerPassArgs を同一トランザクションで
// InsertTx する（ヒント経路。docs/recording.md §3.1「ルールの作成 / 更新 / 削除」）。
// dual-write を避けるため、書き込みが失敗すればこのジョブも一緒にロールバックされる。
//
// h.river が nil の場合は何もしない（テストや、将来 River を持たない api 構成を許容する
// ため。RouterConfig.RiverClient のコメント参照）。RulerPassArgs.InsertOpts の
// UniqueOpts{ByArgs, ByState} により、同一サイトのヒントは定期実行に合流する。
func (h *Server) insertRulerPassHint(ctx context.Context, tx pgx.Tx, site string) error {
	if h.river == nil {
		return nil
	}
	if _, err := h.river.InsertTx(ctx, tx, worker.RulerPassArgs{Site: site}, nil); err != nil {
		return fmt.Errorf("inserting ruler_pass hint for site %s: %w", site, err)
	}
	return nil
}

// ruleTargetSites は rule_sites から対象サイト名の一覧を読む。空（指定なし）は
// そのまま空スライスで返す --- 「全サイト」への展開は呼び出し元
// （insertRulerPassHintsForRuleSites）の責務にする。
func ruleTargetSites(ctx context.Context, q *sqlcgen.Queries, ruleID int64) ([]string, error) {
	rows, err := q.ListRuleSites(ctx, ruleID)
	if err != nil {
		return nil, fmt.Errorf("listing rule sites for rule %d: %w", ruleID, err)
	}
	sites := make([]string, 0, len(rows))
	for _, r := range rows {
		sites = append(sites, r.Site)
	}
	return sites, nil
}

// insertRulerPassHintsForRuleSites はルールが対象とする各サイトに ruler_pass ヒントを
// 投入する（issue #184 M4-12「含むもの」3）。sites は rule_sites から読んだ対象一覧で、
// 空（指定なし = 全サイト）なら h.siteNames（レジストリの全 site）に展開する
// （docs/schema/rules.md「rule_sites 未指定 = 全サイト」と同じ規約）。
//
// api は site に束縛されない（不変条件 1）ため、1 プロセスがレジストリの全サイトに
// ヒントを投入できる。呼び出し元（CreateRule/UpdateRule）はルールの子表書き込みと
// 同一トランザクションで呼ぶこと。DeleteRule は rule_sites が ON DELETE CASCADE で
// 消える前に対象サイトを読んでおく必要がある。
func (h *Server) insertRulerPassHintsForRuleSites(ctx context.Context, tx pgx.Tx, sites []string) error {
	if len(sites) == 0 {
		sites = h.siteNames
	}
	for _, site := range sites {
		if err := h.insertRulerPassHint(ctx, tx, site); err != nil {
			return err
		}
	}
	return nil
}

// validateEncodeProfiles は encodeProfiles の各名前が config 定義に存在することを
// 検査する。ルール保存と予約 overrides の両方で使う（issue #64「ルール / overrides
// 保存時に未知プロファイル名を拒否」）。
//
// h.encodeProfiles が nil のとき（RouterConfig.EncodeProfileNames 未設定 = テストの
// 部分構成）は名前検証をスキップする。空 map（len=0 だが non-nil）は「プロファイルが
// 1 つも無い」とみなし、どんな名前も未知として弾く。
func (h *Server) validateEncodeProfiles(names []string) error {
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

// validateRuleSites は sites の各要素が site レジストリ（config.mirakc/mirakcs、
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

// savedRuleSites は ruleID に保存済みの rule_sites を集合で返す。
// 存在しないルールでは空集合になる（404 判定は呼び出し側の GetRule が担う）。
func savedRuleSites(ctx context.Context, q *sqlcgen.Queries, ruleID int64) (map[string]struct{}, error) {
	rows, err := q.ListRuleSites(ctx, ruleID)
	if err != nil {
		return nil, fmt.Errorf("listing saved rule sites: %w", err)
	}
	set := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		set[r.Site] = struct{}{}
	}
	return set, nil
}

// validateRuleInput はルール create/update の入力を検査する。
// encodeProfiles の存在検証は h.encodeProfiles（config から注入）を使う。
// 集合が nil のときは名前検証をスキップする（テストの部分構成）。
// savedSites は sites のレジストリ照合を免除する保存済みの site 名（create では nil。
// validateRuleSites 参照）。
func (h *Server) validateRuleInput(ctx context.Context, in RuleInput, savedSites map[string]struct{}) error {
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
	// dedupeThreshold は pg_trgm の similarity() と比較される（internal/ruler/dedupe.go の
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
	// dedupeWindowSeconds は interval に変換され、internal/ruler/dedupe.go の
	// "rec.program_start_at >= now() - c.dedupe_window" の右辺に使われる。
	//
	// **0 も弾く。** 0 は「時間窓なし」ではない —— 窓なしは NULL（列を省略する）であり、
	// 0 を入れると条件が "program_start_at >= now()" になって、比較対象が必ず過去の
	// 放送である以上つねに偽になる。重複排除が黙って無効化されるという、負値とまったく
	// 同じ失敗モードである（dedupeThreshold の 0 を弾くのと対称）。窓を設けたくない
	// なら値を送らない。
	if in.DedupeWindowSeconds != nil && *in.DedupeWindowSeconds <= 0 {
		return fmt.Errorf("dedupeWindowSeconds must be > 0 (0 is not \"no window\" — omit the field for that; "+
			"0 makes the window condition always false and silently disables dedupe), got %d", *in.DedupeWindowSeconds)
	}
	if in.TextMatches != nil {
		q := sqlcgen.New(h.pool)
		for _, m := range *in.TextMatches {
			if m.Mode == Regex && m.Value != "" {
				if err := q.ValidateRegexPattern(ctx, m.Value); err != nil {
					return fmt.Errorf("invalid regex %q (POSIX ARE; lookbehind is not supported): %w", m.Value, err)
				}
			}
		}
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

func insertRule(ctx context.Context, q *sqlcgen.Queries, in RuleInput) (sqlcgen.Rule, error) {
	params, err := ruleCreateParams(in)
	if err != nil {
		return sqlcgen.Rule{}, err
	}
	return q.CreateRule(ctx, params)
}

func ruleCreateParams(in RuleInput) (sqlcgen.CreateRuleParams, error) {
	p := sqlcgen.CreateRuleParams{
		Name:             strings.TrimSpace(in.Name),
		Description:      derefStr(in.Description),
		Enabled:          true,
		Priority:         10,
		KeepOriginal:     "always",
		EncodeProfiles:   []string{},
		FilenameTemplate: "",
		Metadata:         json.RawMessage("{}"),
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.Priority != nil {
		p.Priority = int32(*in.Priority)
	}
	if in.IsFree != nil {
		p.IsFree = pgtype.Bool{Bool: *in.IsFree, Valid: true}
	}
	p.DurationMinMs = in.DurationMinMs
	p.DurationMaxMs = in.DurationMaxMs
	if in.PeriodStartAt != nil {
		t := *in.PeriodStartAt
		p.PeriodStartAt = &t
	}
	if in.PeriodEndAt != nil {
		t := *in.PeriodEndAt
		p.PeriodEndAt = &t
	}
	if in.DedupeEnabled != nil {
		p.DedupeEnabled = *in.DedupeEnabled
	}
	if in.DedupeThreshold != nil {
		p.DedupeThreshold = pgtype.Float4{Float32: *in.DedupeThreshold, Valid: true}
	}
	if in.DedupeWindowSeconds != nil {
		p.DedupeWindow = secondsToInterval(*in.DedupeWindowSeconds)
	}
	if in.KeepOriginal != nil {
		p.KeepOriginal = string(*in.KeepOriginal)
	}
	if in.EncodeProfiles != nil {
		p.EncodeProfiles = canonicalizeStrings(*in.EncodeProfiles)
	}
	if in.FilenameTemplate != nil {
		p.FilenameTemplate = *in.FilenameTemplate
	}
	if in.Metadata != nil {
		b, err := json.Marshal(in.Metadata)
		if err != nil {
			return p, fmt.Errorf("marshalling metadata: %w", err)
		}
		p.Metadata = b
	}
	return p, nil
}

func ruleUpdateParams(id int64, in RuleInput) (sqlcgen.UpdateRuleParams, error) {
	c, err := ruleCreateParams(in)
	if err != nil {
		return sqlcgen.UpdateRuleParams{}, err
	}
	return sqlcgen.UpdateRuleParams{
		Name:             c.Name,
		Description:      c.Description,
		Enabled:          c.Enabled,
		Priority:         c.Priority,
		IsFree:           c.IsFree,
		DurationMinMs:    c.DurationMinMs,
		DurationMaxMs:    c.DurationMaxMs,
		PeriodStartAt:    c.PeriodStartAt,
		PeriodEndAt:      c.PeriodEndAt,
		DedupeEnabled:    c.DedupeEnabled,
		DedupeThreshold:  c.DedupeThreshold,
		DedupeWindow:     c.DedupeWindow,
		KeepOriginal:     c.KeepOriginal,
		EncodeProfiles:   c.EncodeProfiles,
		FilenameTemplate: c.FilenameTemplate,
		Metadata:         c.Metadata,
		ID:               id,
	}, nil
}

func replaceRuleChildren(ctx context.Context, q *sqlcgen.Queries, ruleID int64, in RuleInput) error {
	if err := q.DeleteRuleTextMatches(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleServices(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleChannelTypes(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleGenres(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleTimes(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleSites(ctx, ruleID); err != nil {
		return err
	}

	if in.TextMatches != nil {
		for i, m := range *in.TextMatches {
			if err := q.InsertRuleTextMatch(ctx, sqlcgen.InsertRuleTextMatchParams{
				RuleID:        ruleID,
				Seq:           int32(i),
				Target:        string(m.Target),
				Mode:          string(m.Mode),
				Value:         m.Value,
				CaseSensitive: derefBool(m.CaseSensitive),
				Negate:        derefBool(m.Negate),
			}); err != nil {
				return fmt.Errorf("inserting text match %d: %w", i, err)
			}
		}
	}
	if in.Services != nil {
		for _, s := range *in.Services {
			if err := q.InsertRuleService(ctx, sqlcgen.InsertRuleServiceParams{
				RuleID:    ruleID,
				NetworkID: int32(s.NetworkId),
				ServiceID: int32(s.ServiceId),
			}); err != nil {
				return fmt.Errorf("inserting service: %w", err)
			}
		}
	}
	if in.ChannelTypes != nil {
		for _, ct := range *in.ChannelTypes {
			if err := q.InsertRuleChannelType(ctx, sqlcgen.InsertRuleChannelTypeParams{
				RuleID:      ruleID,
				ChannelType: string(ct),
			}); err != nil {
				return fmt.Errorf("inserting channel type: %w", err)
			}
		}
	}
	if in.Genres != nil {
		for _, g := range *in.Genres {
			if err := q.InsertRuleGenre(ctx, sqlcgen.InsertRuleGenreParams{
				RuleID:   ruleID,
				GenreLv1: int16(g),
			}); err != nil {
				return fmt.Errorf("inserting genre: %w", err)
			}
		}
	}
	if in.Times != nil {
		for i, tw := range *in.Times {
			if err := q.InsertRuleTime(ctx, sqlcgen.InsertRuleTimeParams{
				RuleID:   ruleID,
				Seq:      int32(i),
				Weekdays: int32(tw.Weekdays),
				StartSec: int32(tw.StartSec),
				EndSec:   int32(tw.EndSec),
			}); err != nil {
				return fmt.Errorf("inserting time window %d: %w", i, err)
			}
		}
	}
	if in.Sites != nil {
		for _, site := range *in.Sites {
			// validateRuleSites がすでに空文字列を 400 で拒否しているので、
			// CreateRule/UpdateRule 経由では到達しない。将来この関数に検証を通らない
			// 経路が増えたときは黙って落とさずエラーにする（黙って落とすと
			// 「保存は成功したが絞り込み条件が消えている」になる）。
			if site == "" {
				return errors.New("rule site must not be empty")
			}
			if err := q.InsertRuleSite(ctx, sqlcgen.InsertRuleSiteParams{
				RuleID: ruleID,
				Site:   site,
			}); err != nil {
				return fmt.Errorf("inserting site: %w", err)
			}
		}
	}
	return nil
}

func loadRule(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.Rule) (Rule, error) {
	r := ruleFromRow(row)

	texts, err := q.ListRuleTextMatches(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(texts) > 0 {
		ms := make([]RuleTextMatch, 0, len(texts))
		for _, t := range texts {
			cs, neg := t.CaseSensitive, t.Negate
			ms = append(ms, RuleTextMatch{
				Target:        RuleTextMatchTarget(t.Target),
				Mode:          RuleTextMatchMode(t.Mode),
				Value:         t.Value,
				CaseSensitive: &cs,
				Negate:        &neg,
			})
		}
		r.TextMatches = &ms
	}

	services, err := q.ListRuleServices(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(services) > 0 {
		ss := make([]RuleService, 0, len(services))
		for _, s := range services {
			ss = append(ss, RuleService{NetworkId: int(s.NetworkID), ServiceId: int(s.ServiceID)})
		}
		r.Services = &ss
	}

	cts, err := q.ListRuleChannelTypes(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(cts) > 0 {
		out := make([]RuleChannelTypes, 0, len(cts))
		for _, c := range cts {
			out = append(out, RuleChannelTypes(c.ChannelType))
		}
		r.ChannelTypes = &out
	}

	genres, err := q.ListRuleGenres(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(genres) > 0 {
		gs := make([]int, 0, len(genres))
		for _, g := range genres {
			gs = append(gs, int(g.GenreLv1))
		}
		r.Genres = &gs
	}

	times, err := q.ListRuleTimes(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(times) > 0 {
		ts := make([]RuleTimeWindow, 0, len(times))
		for _, t := range times {
			ts = append(ts, RuleTimeWindow{
				Weekdays: int(t.Weekdays),
				StartSec: int(t.StartSec),
				EndSec:   int(t.EndSec),
			})
		}
		r.Times = &ts
	}

	sites, err := q.ListRuleSites(ctx, row.ID)
	if err != nil {
		return r, err
	}
	if len(sites) > 0 {
		ss := make([]string, 0, len(sites))
		for _, s := range sites {
			ss = append(ss, s.Site)
		}
		r.Sites = &ss
	}

	return r, nil
}

func ruleFromRow(row sqlcgen.Rule) Rule {
	desc := row.Description
	ft := row.FilenameTemplate
	r := Rule{
		Id:               row.ID,
		Name:             row.Name,
		Description:      &desc,
		Enabled:          row.Enabled,
		Priority:         int(row.Priority),
		KeepOriginal:     RuleKeepOriginal(row.KeepOriginal),
		FilenameTemplate: &ft,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
	if row.IsFree.Valid {
		v := row.IsFree.Bool
		r.IsFree = &v
	}
	r.DurationMinMs = row.DurationMinMs
	r.DurationMaxMs = row.DurationMaxMs
	r.PeriodStartAt = row.PeriodStartAt
	r.PeriodEndAt = row.PeriodEndAt
	de := row.DedupeEnabled
	r.DedupeEnabled = &de
	if row.DedupeThreshold.Valid {
		v := row.DedupeThreshold.Float32
		r.DedupeThreshold = &v
	}
	if secs, ok := intervalToSeconds(row.DedupeWindow); ok {
		r.DedupeWindowSeconds = &secs
	}
	if len(row.EncodeProfiles) > 0 {
		ep := slices.Clone(row.EncodeProfiles)
		r.EncodeProfiles = &ep
	}
	if len(row.Metadata) > 0 && string(row.Metadata) != "{}" && string(row.Metadata) != "null" {
		var m map[string]interface{}
		if json.Unmarshal(row.Metadata, &m) == nil {
			r.Metadata = &m
		}
	}
	return r
}

func secondsToInterval(sec int64) pgtype.Interval {
	return pgtype.Interval{
		Microseconds: sec * 1_000_000,
		Valid:        true,
	}
}

func intervalToSeconds(iv pgtype.Interval) (int64, bool) {
	if !iv.Valid {
		return 0, false
	}
	return iv.Microseconds / 1_000_000, true
}

func canonicalizeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefBool(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}
