// Package capacity はチューナー容量の超過区間を求める（issue #24 M2-10、
// issue #21 / docs/data.md §6.5）。
//
// # 主張は下界に限る
//
// 「この区間は超過している」は確実だが、**返らなかった区間が「収まる」ことは
// 保証しない**。Rokuban から見えない消費者（並走する EPGStation・ライブ視聴・
// mirakc の EPG 収集ジョブ）がいるうえ、mirakc の excluded_channels は
// /api/tuners に載らないため、既知の盲点はすべて「警告を見逃す」方向に偏っている。
// したがってこのパッケージは**区間の性質だけを返し、どの予約・どの番組が負けるかは
// 一切主張しない**。勝敗を決めるのは mirakc であり、Rokuban には見えない消費者が
// いるので予測できない。
//
// 予約ごとの競合フラグ・優先度による貪欲詰め・負ける側の決定は実装しない
// （EPGStation 式の実装から消えるもの。docs/data.md §6.5）。
//
// # 判定
//
// 需要の単位は予約件数ではなく**異なる物理チャンネル数**（同一物理チャンネルなら
// 1 本のチューナーに相乗りできる。副産物としてマルチ編成のサブサービスが自然に
// 畳まれる）。判定は種別部分集合に縮約した Hall 条件:
//
//	∀A ⊆ {GR,BS,CS,SKY}:  Σ_{t∈A} d[t] ≤ cap(A)
//
// 縮約が成り立つのは、同じ channel_type の需要がチューナーから見て隣接集合が
// 完全に一致する（twin vertices）ため。破れた A から不足本数と詰まった種別が
// 副産物として出る。
package capacity

import (
	"math/bits"
	"slices"
	"time"
)

// ChannelTypes は Hall 条件で走査する種別。mirakc の channelTypes と
// tuner_sync.types / reservations.channel_type の CHECK 制約に対応する。
//
// 添字がビット位置になる（GR=1, BS=2, CS=4, SKY=8）。並びは UI に出る
// jammedTypes の順序も決めるので、放送種別として自然な順に固定する。
var ChannelTypes = []string{"GR", "BS", "CS", "SKY"}

// typeCount は種別数。非空部分集合は 2^4 − 1 = 15 通り。
const typeCount = 4

// Demand は 1 予約分の需要。
//
// 予約 1 件が需要 1 とは限らない --- 同一の (Site, ChannelType, Channel) を持つ
// 重なった予約は 1 本のチューナーに相乗りできるので、まとめて需要 1 になる。
// この畳み込みは Compute が行う。
//
// Site は分割キーであって需要のキーではない。判定はサイトごとに独立に行われ
// （N 予約の決定により二部グラフがサイトごとに非連結になる。docs/recording.md §3.1）、
// 分割の内側では (ChannelType, Channel) が物理チャンネルを一意に定める。
type Demand struct {
	Site        string
	ChannelType string
	Channel     string
	StartAt     time.Time
	EndAt       time.Time
}

// Tuner は射影されたチューナー 1 本（tuner_sync の 1 行に対応）。
type Tuner struct {
	Site  string
	Types []string

	// IsAvailable / IsFault は cap(A) に数えるかを決める。
	// 数えるのは**存在して恒久的に壊れていない**チューナー（docs/data.md §6.5）。
	IsAvailable bool
	IsFault     bool
}

// countable は cap(A) に数えるチューナーかを返す。
//
// 設定で無効化（is_available = false）・故障報告あり（is_fault = true）は除外する。
// 一方で**現在の利用者（users / isFree / isUsing）は引かない** --- 一時的な占有で
// あり将来の区間の容量とは無関係なので、そもそも射影していない
// （mirakc.Tuner のコメント参照）。
func (t Tuner) countable() bool { return t.IsAvailable && !t.IsFault }

// Overage は容量が不足している区間 1 つ。
//
// 区間の性質だけを表す。**どの番組・どの予約が負けるかは主張しない。**
type Overage struct {
	Site    string
	StartAt time.Time
	EndAt   time.Time

	// Shortfall は不足本数。破れた種別部分集合 A の Σ_{t∈A} d[t] − cap(A)。
	// 常に 1 以上（0 なら超過していないので区間そのものが作られない）。
	Shortfall int

	// JammedTypes は Hall 条件を破った部分集合 A。「BS が 1 本不足」と言うための材料。
	// ChannelTypes の順に並ぶ。
	JammedTypes []string
}

// Compute は需要とチューナー射影から超過区間の**結合済み**リストを返す。
//
// 地平線全体を 1 回走る（窓ごとに解かない。docs/data.md §6.5）。結果は
// (Site, StartAt) 昇順で、同一サイト内の区間は重ならない。
//
// # 射影が空のサイトは判定しない
//
// tuner_sync に 1 行も無いサイトは**何も主張しない**（超過区間を返さない）。
// 「チューナーが 0 本」と「まだ射影していない / 同期が失敗している」を射影からは
// 区別できず、後者を容量ゼロとして扱うと全区間が超過になって警告が洪水になる。
// 既知の盲点をすべて「警告を見逃す」方向に揃える（docs/data.md §6.5）という
// 性質を、我々自身の無知で崩さないための規律。EpgSyncWorker が空レスポンスで
// スイープを見送るのと同じ姿勢。
//
// 逆に、行はあるが countable なチューナーが 0 本（全台が無効化・故障）の場合は
// 判定する --- それは射影された事実であって我々の無知ではない。
func Compute(demands []Demand, tuners []Tuner) []Overage {
	tunersBySite := make(map[string][]Tuner)
	for _, t := range tuners {
		tunersBySite[t.Site] = append(tunersBySite[t.Site], t)
	}

	demandsBySite := make(map[string][]Demand)
	for _, d := range demands {
		// 長さ 0 の需要はどの瞬間も占有しない（半開区間なので空集合）。
		//
		// これは飾りではなく、同時刻の中での処理順を決めなくてよくする前提。
		// 長さ 0 の需要が残っていると +1 と −1 が同じバッチ・同じチャンネルに並び、
		// −1 が先に来ると cnt が一時的に負になる（0↔1 遷移の判定が
		// `== 1` / `== 0` の等号なので値は壊れないが、負の在庫という
		// 意味のない状態を作る）。入口で落として不変条件 cnt ≥ 0 を保つ。
		if !d.EndAt.After(d.StartAt) {
			continue
		}
		demandsBySite[d.Site] = append(demandsBySite[d.Site], d)
	}

	sites := make([]string, 0, len(demandsBySite))
	for site := range demandsBySite {
		if len(tunersBySite[site]) == 0 {
			continue
		}
		sites = append(sites, site)
	}
	slices.Sort(sites)

	var out []Overage
	for _, site := range sites {
		out = append(out, computeSite(site, demandsBySite[site], capacities(tunersBySite[site]))...)
	}
	return out
}

// capacities は種別部分集合 A ごとの cap(A) を返す（添字は A のビットマスク）。
//
// cap(A) は「A のいずれかの種別に対応するチューナー本数」なので、和ではなく
// **和集合の要素数**。types が空のチューナーはどの A にも数えられない
// （migrations/00015_tuner_sync.sql: 無害なので CHECK で禁止しない）。
func capacities(tuners []Tuner) [1 << typeCount]int {
	var caps [1 << typeCount]int
	for _, t := range tuners {
		if !t.countable() {
			continue
		}
		tm := typeMask(t.Types)
		if tm == 0 {
			continue
		}
		for a := 1; a < 1<<typeCount; a++ {
			if a&int(tm) != 0 {
				caps[a]++
			}
		}
	}
	return caps
}

// typeMask は種別名の集合をビットマスクにする。未知の種別は無視する
// （未知の上流データで判定全体を落とさない。EpgSyncWorker の validChannelTypes と同じ規律）。
func typeMask(types []string) uint8 {
	var m uint8
	for _, name := range types {
		if i := slices.Index(ChannelTypes, name); i >= 0 {
			m |= 1 << uint(i)
		}
	}
	return m
}

// verdict は 1 区間の判定結果。破れた A のうち最悪のものを表す。
type verdict struct {
	shortfall int
	mask      uint8
}

// event は累積和の増減点。
type event struct {
	at      time.Time
	channel channelKey
	typeIdx int
	delta   int
}

// channelKey は物理チャンネル。サイトで分割した内側では (channelType, channel) で一意
// （GR 27ch が東京と高松で別物であることは分割が処理する。docs/data.md §6.5）。
type channelKey struct {
	channelType string
	channel     string
}

// computeSite は 1 サイト分の超過区間を求める。
//
// **純粋な imos 法（予約に ±1 して累積和）は誤り。** 需要は和ではなく異なり数なので、
// 累積和はチャンネル単位に張り、0→1 / 1→0 の遷移でのみ d[t] を増減させる
// （同一物理チャンネルの 2 予約は需要 1）。
//
// 同時刻のイベントは**まとめて反映してから**判定する。1 件ずつ判定すると、
// 同じチャンネルの番組が連続するとき（10:00 に終わって 10:00 に始まる）に
// d が一瞬下がった中間状態を区間として観測してしまう。
func computeSite(site string, demands []Demand, caps [1 << typeCount]int) []Overage {
	events := make([]event, 0, 2*len(demands))
	for _, d := range demands {
		ti := slices.Index(ChannelTypes, d.ChannelType)
		if ti < 0 {
			// 未知の種別はどのチューナーにも載せられない。数えないことで
			// 「警告を見逃す」側に倒す（判定全体を落とさない）。
			continue
		}
		ch := channelKey{channelType: d.ChannelType, channel: d.Channel}
		events = append(events,
			event{at: d.StartAt, channel: ch, typeIdx: ti, delta: +1},
			event{at: d.EndAt, channel: ch, typeIdx: ti, delta: -1},
		)
	}
	// 時刻順に並べるだけで、同時刻の中での順序は決めない。**区間が半開である
	// （10:00 に終わる需要と 10:00 に始まる需要は重なっていない）ことを保証するのは、
	// 同時刻のイベントをまとめて反映してから判定する下のループ**であって、
	// 開始/終了の処理順ではない。実際に「終了を先に」という並べ替えを入れても
	// 入れなくても結果は変わらない（どちらの順でも d はバッチの終わりで同じ値に
	// 落ち着く）ので、意味のない規則を置かない。
	slices.SortFunc(events, func(a, b event) int { return a.at.Compare(b.at) })

	var (
		out     []Overage
		cnt     = make(map[channelKey]int)
		d       [typeCount]int
		current verdict // いま観測している区間の判定（shortfall == 0 は「超過していない」）
		open    *Overage
	)

	for i := 0; i < len(events); {
		at := events[i].at
		changed := false
		for ; i < len(events) && events[i].at.Equal(at); i++ {
			ev := events[i]
			cnt[ev.channel] += ev.delta
			switch {
			case ev.delta > 0 && cnt[ev.channel] == 1:
				d[ev.typeIdx]++ // 0→1: 新たに 1 本必要になった
				changed = true
			case ev.delta < 0 && cnt[ev.channel] == 0:
				d[ev.typeIdx]-- // 1→0: 不要になった
				changed = true
			}
		}
		// d が変わらなければ判定は変わらないので回す必要すらない
		// （チャンネル共有が効くほど計算量が落ちる。docs/data.md §6.5）。
		if !changed {
			continue
		}

		v := evaluate(d, caps)
		if v == current {
			continue
		}
		// 判定が変わった時刻が、いま開いている区間の終わりになる。
		// 隣接する同じ判定の区間はここで自然に結合される。
		if open != nil {
			open.EndAt = at
			out = append(out, *open)
			open = nil
		}
		if v.shortfall > 0 {
			open = &Overage{
				Site:        site,
				StartAt:     at,
				Shortfall:   v.shortfall,
				JammedTypes: typeNames(v.mask),
			}
		}
		current = v
	}

	// 最後のイベントの後は全チャンネルの cnt が 0 なので d も 0 になり、
	// Σd − cap ≤ 0 で必ず区間が閉じている。開いたまま残ることはない。
	return out
}

// evaluate は 15 通りの種別部分集合を調べ、最も破れている A を返す。
//
// 破れた A が複数あるときは (1) 不足本数が大きいもの (2) 要素数が少ないもの
// (3) ビットマスクが小さいもの を選ぶ。要素数が少ない A の方が「BS が 1 本不足」の
// ように詰まった箇所を狭く言い当てるため。(3) は結果を決定的にするためだけの規則。
func evaluate(d [typeCount]int, caps [1 << typeCount]int) verdict {
	best := verdict{}
	for a := 1; a < 1<<typeCount; a++ {
		sum := 0
		for i := range typeCount {
			if a&(1<<uint(i)) != 0 {
				sum += d[i]
			}
		}
		shortfall := sum - caps[a]
		if shortfall <= 0 {
			continue
		}
		if shortfall < best.shortfall {
			continue
		}
		if shortfall == best.shortfall && bits.OnesCount8(uint8(a)) >= bits.OnesCount8(best.mask) {
			continue
		}
		best = verdict{shortfall: shortfall, mask: uint8(a)}
	}
	return best
}

// typeNames はビットマスクを ChannelTypes の順の種別名に戻す。
func typeNames(mask uint8) []string {
	names := make([]string, 0, typeCount)
	for i, name := range ChannelTypes {
		if mask&(1<<uint(i)) != 0 {
			names = append(names, name)
		}
	}
	return names
}

// Intersecting は半開区間 [start, end) と重なる超過区間だけを返す。
//
// Compute が地平線全体を 1 回解いた結果の上での範囲検索（グリッドの帯・
// 予約一覧のバッジ・メトリクスはすべてこの形に落ちる。docs/data.md §6.5）。
// 判定は半開区間（`a.start < b.end AND b.start < a.end`）で行う ---
// 閉区間だと窓の端にちょうど接する区間が重なりとして返る。
func Intersecting(overages []Overage, start, end time.Time) []Overage {
	out := make([]Overage, 0, len(overages))
	for _, o := range overages {
		if o.StartAt.Before(end) && o.EndAt.After(start) {
			out = append(out, o)
		}
	}
	return out
}
