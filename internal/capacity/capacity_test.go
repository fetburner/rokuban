package capacity

import (
	"reflect"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 1, 19, 0, 0, 0, time.UTC)

// at は base からの分オフセット。
func at(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

// gr / bs は需要を 1 件作るヘルパー。
func demand(channelType, channel string, startMin, endMin int) Demand {
	return Demand{
		Site:        "default",
		ChannelType: channelType,
		Channel:     channel,
		StartAt:     at(startMin),
		EndAt:       at(endMin),
	}
}

// tuner は countable なチューナーを 1 本作る。
func tuner(types ...string) Tuner {
	return Tuner{Site: "default", Types: types, IsAvailable: true, IsFault: false}
}

// summary は Overage を比較しやすい形に落とす（時刻は base からの分）。
type summary struct {
	site      string
	startMin  int
	endMin    int
	shortfall int
	jammed    string
}

func summarize(t *testing.T, overages []Overage) []summary {
	t.Helper()
	out := make([]summary, 0, len(overages))
	for _, o := range overages {
		jammed := ""
		for i, name := range o.JammedTypes {
			if i > 0 {
				jammed += "+"
			}
			jammed += name
		}
		out = append(out, summary{
			site:      o.Site,
			startMin:  int(o.StartAt.Sub(base) / time.Minute),
			endMin:    int(o.EndAt.Sub(base) / time.Minute),
			shortfall: o.Shortfall,
			jammed:    jammed,
		})
	}
	return out
}

// assertOverages は期待する区間の一致に加えて、返り値が常に満たすべき不変条件も
// 確かめる。openapi.yaml の CapacityOverage が約束しているのはこの形
// （shortfall は 1 以上、jammedTypes は非空、区間は幅を持つ）。
func assertOverages(t *testing.T, got []Overage, want []summary) {
	t.Helper()
	for i, o := range got {
		if !o.EndAt.After(o.StartAt) {
			t.Errorf("overage[%d] has no width: %v..%v", i, o.StartAt, o.EndAt)
		}
		if o.Shortfall < 1 {
			t.Errorf("overage[%d] shortfall = %d, want >= 1", i, o.Shortfall)
		}
		if len(o.JammedTypes) == 0 {
			t.Errorf("overage[%d] has no jammed types", i)
		}
		if i > 0 && got[i-1].Site == o.Site && o.StartAt.Before(got[i-1].EndAt) {
			t.Errorf("overage[%d] overlaps the previous one on the same site", i)
		}
	}
	gotSummary := summarize(t, got)
	if len(want) == 0 && len(gotSummary) == 0 {
		return
	}
	if !reflect.DeepEqual(gotSummary, want) {
		t.Errorf("overages =\n  %+v\nwant\n  %+v", gotSummary, want)
	}
}

// GR 専用 1 本 + GR/BS 両対応 1 本という構成で、Hall 条件の縮約が効くことを確かめる
// （docs/data.md §6.5 の検算表、issue #21）。
//
// **「素朴な重なり数 vs 総本数」では検出できないケースを含む。** GR 1 + BS 2 は
// 総本数（2）が足りているのに {BS}: 2 ≤ 1 が破れる。縮約を実装していないと
// このケースを見逃す。
func TestCompute_HallReductionWithSharedTuner(t *testing.T) {
	tuners := []Tuner{tuner("GR"), tuner("GR", "BS")}

	tests := []struct {
		name    string
		demands []Demand
		want    []summary
	}{
		{
			// {GR}:1≤2 / {BS}:1≤1 / {GR,BS}:2≤2 → 収まる（T2→BS, T1→GR）
			name: "GR 1 + BS 1 は収まる",
			demands: []Demand{
				demand("GR", "27", 0, 60),
				demand("BS", "BS15_0", 0, 60),
			},
			want: nil,
		},
		{
			// {GR,BS}: 3≤2 が破れる
			name: "GR 2 + BS 1 は 1 本不足（両種別で詰まる）",
			demands: []Demand{
				demand("GR", "27", 0, 60),
				demand("GR", "25", 0, 60),
				demand("BS", "BS15_0", 0, 60),
			},
			want: []summary{{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR+BS"}},
		},
		{
			// {BS}: 2≤1 が破れる。総本数（2）は足りているので、素朴な
			// 「重なり数 vs 総本数」では検出できない。
			name: "GR 1 + BS 2 は総本数が足りていても BS で詰まる",
			demands: []Demand{
				demand("GR", "27", 0, 60),
				demand("BS", "BS15_0", 0, 60),
				demand("BS", "BS03_1", 0, 60),
			},
			want: []summary{{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "BS"}},
		},
		{
			// BS だけで 3 本必要だが BS 対応は 1 本しかない。
			name: "BS 3 は 2 本不足",
			demands: []Demand{
				demand("BS", "BS15_0", 0, 60),
				demand("BS", "BS03_1", 0, 60),
				demand("BS", "BS09_0", 0, 60),
			},
			want: []summary{{site: "default", startMin: 0, endMin: 60, shortfall: 2, jammed: "BS"}},
		},
		{
			// GR 対応は 2 本あるので GR 2 は収まる。
			name: "GR 2 だけなら収まる",
			demands: []Demand{
				demand("GR", "27", 0, 60),
				demand("GR", "25", 0, 60),
			},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertOverages(t, Compute(tc.demands, tuners), tc.want)
		})
	}
}

// 需要の単位は予約件数ではなく異なる物理チャンネル数。同一物理チャンネルの
// 複数予約は 1 本のチューナーに相乗りできるので需要 1 になる
// （純粋な imos 法だと 2 と数えて誤検出する）。
func TestCompute_SamePhysicalChannelSharesOneTuner(t *testing.T) {
	tuners := []Tuner{tuner("GR")}

	t.Run("同一チャンネルの 3 予約は需要 1 で超過しない", func(t *testing.T) {
		demands := []Demand{
			demand("GR", "27", 0, 60),
			demand("GR", "27", 10, 40),
			demand("GR", "27", 30, 90),
		}
		assertOverages(t, Compute(demands, tuners), nil)
	})

	// 反対方向: 別チャンネルなら同じ本数の予約でも超過する。
	t.Run("別チャンネルの 2 予約は需要 2 で超過する", func(t *testing.T) {
		demands := []Demand{
			demand("GR", "27", 0, 60),
			demand("GR", "25", 10, 40),
		}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 10, endMin: 40, shortfall: 1, jammed: "GR"},
		})
	})

	// マルチ編成のサブサービスは同一物理チャンネルなので自然に畳まれる
	// （副産物。docs/data.md §6.5）。channel が同じで channel_type も同じなら
	// service が違っても需要 1。
	t.Run("マルチ編成のサブサービスは畳まれる", func(t *testing.T) {
		demands := []Demand{
			demand("GR", "27", 0, 60), // ＮＨＫ総合１
			demand("GR", "27", 0, 60), // ＮＨＫ総合２（同一物理チャンネル）
		}
		assertOverages(t, Compute(demands, tuners), nil)
	})
}

// 区間は半開。10:00 に終わる需要と 10:00 に始まる需要は重なっていない。
func TestCompute_HalfOpenIntervals(t *testing.T) {
	tuners := []Tuner{tuner("GR")}

	t.Run("背中合わせの 2 予約は超過しない", func(t *testing.T) {
		demands := []Demand{
			demand("GR", "27", 0, 30),
			demand("GR", "25", 30, 60),
		}
		assertOverages(t, Compute(demands, tuners), nil)
	})

	t.Run("1 分でも重なれば超過する", func(t *testing.T) {
		demands := []Demand{
			demand("GR", "27", 0, 31),
			demand("GR", "25", 30, 60),
		}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 30, endMin: 31, shortfall: 1, jammed: "GR"},
		})
	})

	// 同一チャンネルの連続番組。同時刻のイベントをまとめて反映しないと、
	// d が一瞬下がった中間状態を区間として観測してしまう。
	t.Run("同一チャンネルの連続番組で中間状態を観測しない", func(t *testing.T) {
		demands := []Demand{
			demand("GR", "27", 0, 30),
			demand("GR", "27", 30, 60),
			demand("GR", "25", 0, 60),
			demand("GR", "24", 0, 60),
		}
		// GR は 3 チャンネル分必要で 1 本しかないので、全区間で 2 本不足。
		// 30 分の時点で区間が割れてはいけない。
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 2, jammed: "GR"},
		})
	})
}

// 隣接する同じ判定の区間は結合され、判定が変わるところで割れる。
func TestCompute_MergesAdjacentIntervals(t *testing.T) {
	tuners := []Tuner{tuner("GR")}

	t.Run("同じ不足本数が続く区間は 1 本に結合される", func(t *testing.T) {
		// [0,60) に 27ch、[30,90) に 25ch、[60,120) に 24ch。
		// 需要は 30-60 で 2、60-90 で 2、90-120 で 1。不足は 30-90 でずっと 1。
		demands := []Demand{
			demand("GR", "27", 0, 60),
			demand("GR", "25", 30, 90),
			demand("GR", "24", 60, 120),
		}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 30, endMin: 90, shortfall: 1, jammed: "GR"},
		})
	})

	t.Run("不足本数が変わると区間が割れる", func(t *testing.T) {
		// [0,60) 27ch / [0,60) 25ch / [20,40) 24ch。
		// 需要は 0-20 で 2（不足 1）、20-40 で 3（不足 2）、40-60 で 2（不足 1）。
		demands := []Demand{
			demand("GR", "27", 0, 60),
			demand("GR", "25", 0, 60),
			demand("GR", "24", 20, 40),
		}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 20, shortfall: 1, jammed: "GR"},
			{site: "default", startMin: 20, endMin: 40, shortfall: 2, jammed: "GR"},
			{site: "default", startMin: 40, endMin: 60, shortfall: 1, jammed: "GR"},
		})
	})

	t.Run("超過が途切れると別の区間になる", func(t *testing.T) {
		demands := []Demand{
			demand("GR", "27", 0, 60),
			demand("GR", "25", 0, 60),
			demand("GR", "24", 120, 180),
			demand("GR", "22", 120, 180),
		}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR"},
			{site: "default", startMin: 120, endMin: 180, shortfall: 1, jammed: "GR"},
		})
	})
}

// cap に数えるのは存在して恒久的に壊れていないチューナーだけ（docs/data.md §6.5）。
func TestCompute_CountsOnlyHealthyTuners(t *testing.T) {
	demands := []Demand{
		demand("GR", "27", 0, 60),
		demand("GR", "25", 0, 60),
	}

	t.Run("2 本とも健全なら収まる", func(t *testing.T) {
		tuners := []Tuner{tuner("GR"), tuner("GR")}
		assertOverages(t, Compute(demands, tuners), nil)
	})

	t.Run("設定で無効化されたチューナーは数えない", func(t *testing.T) {
		tuners := []Tuner{
			tuner("GR"),
			{Site: "default", Types: []string{"GR"}, IsAvailable: false},
		}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR"},
		})
	})

	t.Run("故障報告のあるチューナーは数えない", func(t *testing.T) {
		tuners := []Tuner{
			tuner("GR"),
			{Site: "default", Types: []string{"GR"}, IsAvailable: true, IsFault: true},
		}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR"},
		})
	})

	t.Run("types が空のチューナーはどの cap にも数えない", func(t *testing.T) {
		tuners := []Tuner{tuner("GR"), tuner()}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR"},
		})
	})
}

// 射影が空のサイトは何も主張しない（自分の無知を警告に変えない）。
func TestCompute_NoClaimWithoutTunerProjection(t *testing.T) {
	demands := []Demand{
		demand("GR", "27", 0, 60),
		demand("GR", "25", 0, 60),
	}

	t.Run("射影が 1 行も無ければ超過を主張しない", func(t *testing.T) {
		assertOverages(t, Compute(demands, nil), nil)
	})

	// 反対方向: 行が 1 つでもあれば（全台が壊れていても）判定する。
	// それは射影された事実であって我々の無知ではない。
	t.Run("全台が故障していても行があれば主張する", func(t *testing.T) {
		tuners := []Tuner{{Site: "default", Types: []string{"GR"}, IsAvailable: true, IsFault: true}}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 2, jammed: "GR"},
		})
	})
}

// 判定はサイトごとに独立（N 予約の決定により二部グラフがサイトごとに非連結）。
// site は分割キーであって需要のキーには入らない。
func TestCompute_PerSiteDecomposition(t *testing.T) {
	tokyoGR27 := Demand{Site: "tokyo", ChannelType: "GR", Channel: "27", StartAt: at(0), EndAt: at(60)}
	tokyoGR25 := Demand{Site: "tokyo", ChannelType: "GR", Channel: "25", StartAt: at(0), EndAt: at(60)}
	takamatsuGR27 := Demand{Site: "takamatsu", ChannelType: "GR", Channel: "27", StartAt: at(0), EndAt: at(60)}

	t.Run("他サイトのチューナーで自サイトの需要は賄えない", func(t *testing.T) {
		// tokyo に 1 本、takamatsu に 1 本。tokyo は 2 チャンネル必要なので不足する。
		// 総本数 2 でプールすれば収まるが、予約は site に束縛されている。
		tuners := []Tuner{
			{Site: "tokyo", Types: []string{"GR"}, IsAvailable: true},
			{Site: "takamatsu", Types: []string{"GR"}, IsAvailable: true},
		}
		assertOverages(t, Compute([]Demand{tokyoGR27, tokyoGR25}, tuners), []summary{
			{site: "tokyo", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR"},
		})
	})

	t.Run("同じチャンネル番号でもサイトが違えば別の物理チャンネル", func(t *testing.T) {
		// GR 27ch が東京と高松で 1 件ずつ。各サイト 1 本ずつあるので収まる。
		// site を需要のキーに入れず素朴に (channel_type, channel) で畳むと、
		// 別サイトの需要が同一チャンネルとして相乗りしてしまう。
		tuners := []Tuner{
			{Site: "tokyo", Types: []string{"GR"}, IsAvailable: true},
			{Site: "takamatsu", Types: []string{"GR"}, IsAvailable: true},
		}
		assertOverages(t, Compute([]Demand{tokyoGR27, takamatsuGR27}, tuners), nil)
	})

	t.Run("サイトごとに独立して超過を返す", func(t *testing.T) {
		tuners := []Tuner{
			{Site: "tokyo", Types: []string{"GR"}, IsAvailable: true},
			{Site: "takamatsu", Types: []string{"GR"}, IsAvailable: true},
		}
		demands := []Demand{
			tokyoGR27, tokyoGR25,
			takamatsuGR27,
			{Site: "takamatsu", ChannelType: "GR", Channel: "31", StartAt: at(0), EndAt: at(60)},
		}
		// site 昇順（takamatsu < tokyo）で返る。
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "takamatsu", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR"},
			{site: "tokyo", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR"},
		})
	})
}

// 未知の種別は需要にもチューナーにも数えない（判定全体を落とさない）。
func TestCompute_UnknownChannelTypesAreIgnored(t *testing.T) {
	t.Run("未知の種別の需要は数えない", func(t *testing.T) {
		demands := []Demand{
			demand("GR", "27", 0, 60),
			demand("XX", "99", 0, 60),
		}
		assertOverages(t, Compute(demands, []Tuner{tuner("GR")}), nil)
	})

	t.Run("未知の種別しか持たないチューナーは cap に数えない", func(t *testing.T) {
		demands := []Demand{demand("GR", "27", 0, 60)}
		tuners := []Tuner{{Site: "default", Types: []string{"XX"}, IsAvailable: true}}
		// 行はあるので判定はする。cap(GR) = 0 なので 1 本不足。
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR"},
		})
	})
}

// 長さ 0 の需要はどの瞬間も占有しないので、幅 0 の超過区間も生まれない。
//
// これは**振る舞いを固定する test であって、入口の除外分岐を検証していない**。
// Compute には長さ 0 を無害にする仕組みが 2 つあり（入口での除外と、同時刻の
// イベントをまとめてから判定すること）、除外を外してもバッチ処理側が同じ結果に
// する。除外を残しているのは cnt ≥ 0 を保つため（capacity.go のコメント参照）で、
// その効果は外から観測できない。
func TestCompute_ZeroLengthDemandIsNotDemand(t *testing.T) {
	tuners := []Tuner{tuner("GR")}
	demands := []Demand{
		demand("GR", "27", 0, 60),
		demand("GR", "25", 30, 30),
	}
	assertOverages(t, Compute(demands, tuners), nil)
}

// 3 種別が絡む縮約。SKY を含めた 15 通りが効いていることを確かめる。
func TestCompute_ReductionAcrossThreeTypes(t *testing.T) {
	// T1={GR}, T2={GR,BS}, T3={BS,CS}
	tuners := []Tuner{tuner("GR"), tuner("GR", "BS"), tuner("BS", "CS")}

	t.Run("GR1+BS1+CS1 は収まる", func(t *testing.T) {
		demands := []Demand{
			demand("GR", "27", 0, 60),
			demand("BS", "BS15_0", 0, 60),
			demand("CS", "CS02", 0, 60),
		}
		assertOverages(t, Compute(demands, tuners), nil)
	})

	t.Run("BS1+CS2 は BS+CS で詰まる", func(t *testing.T) {
		// cap({BS,CS}) = 2（T2, T3）。需要 3 なので 1 本不足。
		// cap({CS}) = 1 で需要 2 なので {CS} も 1 本不足だが、要素数の少ない
		// {CS} が選ばれる（詰まった箇所を狭く言い当てる）。
		demands := []Demand{
			demand("BS", "BS15_0", 0, 60),
			demand("CS", "CS02", 0, 60),
			demand("CS", "CS04", 0, 60),
		}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "CS"},
		})
	})

	t.Run("GR2+BS1+CS1 は全体で詰まる", func(t *testing.T) {
		// cap({GR,BS,CS}) = 3、需要 4 → 1 本不足。
		// cap({GR}) = 2 で需要 2、cap({BS,CS}) = 2 で需要 2 なのでそれぞれは収まる。
		demands := []Demand{
			demand("GR", "27", 0, 60),
			demand("GR", "25", 0, 60),
			demand("BS", "BS15_0", 0, 60),
			demand("CS", "CS02", 0, 60),
		}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "GR+BS+CS"},
		})
	})

	t.Run("SKY は対応チューナーが無いので 1 件で詰まる", func(t *testing.T) {
		demands := []Demand{demand("SKY", "SKY01", 0, 60)}
		assertOverages(t, Compute(demands, tuners), []summary{
			{site: "default", startMin: 0, endMin: 60, shortfall: 1, jammed: "SKY"},
		})
	})
}

func TestIntersecting(t *testing.T) {
	overages := []Overage{
		{Site: "default", StartAt: at(0), EndAt: at(60), Shortfall: 1, JammedTypes: []string{"GR"}},
		{Site: "default", StartAt: at(120), EndAt: at(180), Shortfall: 2, JammedTypes: []string{"BS"}},
	}

	tests := []struct {
		name       string
		start, end int
		want       int
	}{
		{name: "窓が全区間を含む", start: -60, end: 240, want: 2},
		{name: "窓が 1 つ目だけを含む", start: 0, end: 60, want: 1},
		{name: "窓が区間の内側", start: 10, end: 20, want: 1},
		{name: "窓が区間に接するだけ（半開なので含まない）", start: 60, end: 120, want: 0},
		{name: "窓が区間の前", start: -120, end: 0, want: 0},
		{name: "窓が区間の後", start: 180, end: 240, want: 0},
		{name: "窓が両方に少しだけ掛かる", start: 59, end: 121, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Intersecting(overages, at(tc.start), at(tc.end))
			if len(got) != tc.want {
				t.Errorf("Intersecting(%d, %d) returned %d overages, want %d (%+v)",
					tc.start, tc.end, len(got), tc.want, summarize(t, got))
			}
		})
	}
}
