package jobs

import (
	"fmt"
	"sort"

	"github.com/fetburner/rokuban/internal/mirakc"
)

// VerifySite は、mirakc かファイルシステムに触るジョブ（処理のワーカー）が処理しようとしている
// ジョブの site（jobSite）が、このプロセスが束縛している mirakc クライアントの集合
// （clients、site → *mirakc.Client。Deps.MirakcClients / `--sites` の束縛）に含まれるかを
// 検査する fail-fast ガード（issue #139。1 プロセスが N site を束縛できるようになった
// issue #532 で map 化した）。
//
// mirakc の record id / programId はインスタンススコープであり、DB の site 列は
// そのスコープの境界を表現するために存在する（issue #31、docs/schema.md
// §1-5）。**一次防御はキュー選択そのもの**: site 単位のキュー（ingest/epg/
// reconciler/watcher）は物理名を site で修飾する（River adapter、issue #185
// M4-13）ため、site A 宛のキューを購読していないプロセスは site B 向けのジョブを
// 購読すらしない（cmd/rokuban.validateSiteBinding が worker.queues とサイト束縛の
// 組み合わせも検査する）。VerifySite はその後ろに立つ**二次防御**で、キュー選択が
// 正しくてもジョブの Args.Site 自体が壊れている経路（手で INSERT した / 将来の
// ヒント投入のバグ / River の一意性判定が旧キューの残骸と合流して args だけが
// 別サイトのまま挿入された、等）を捕まえる。掴んだまま mirakc/FS に触れると、
// A の mirakc に B の id を投げて A の別番組を B の recording としてコミットし
// うる（不変条件 3「コミット = DB 行」への違反。詳細は issue #139 本文）。
//
// **1 プロセスが N site を束縛しうるため、「一致するか」ではなく「clients に
// jobSite のエントリがあるか」を見る。** site 単位のキューは束縛サイトの数だけ
// 物理キューを購読するので、1 つの worker が複数 site のジョブを受け取りうる。
// 見つかれば、呼び出し側がそのまま使う *mirakc.Client を返す（呼び出し側が再度 map を
// 引く必要をなくし、VerifySite が通した site とその後実際に使うクライアントがズレる経路を
// 作らない）。
//
// **jobSite は正規化しない。空文字列は無条件に拒む。** config のバリデーションが
// site 名の非空を要求するため、clients のキーは常に実際の site 名であり、
// 空文字列がキーになることはない。jobSite を store.DefaultSite へ読み替えてから
// 引くと、たまたま "default" という名前の site が束縛されているデプロイでは
// `Site: ""` の（本来壊れている）ジョブが素通りしてしまう。二次防御として拾いたいのは
// まさにこの「args.Site 自体が壊れている経路」なので、jobSite を素の値のまま引く。
//
// 呼び出し側は mirakc/FS に触れる**前**（最初の HTTP 呼び出し・一時ファイル作成等より
// 前）に呼ぶこと。合わないジョブは即座に失敗させ、再試行は River に委ねる
// （同じジョブが必ず自サイトの worker に回る保証はないが、他サイトの worker が
// いずれ拾う。うるさいが安全 --- issue #139 本文の「id が自サイトに存在しない」
// ケースと同じ扱いに揃う）。
//
// この判定が jobs にあるのは、呼び出し元がどのパッケージにあっても同じ理由
// （ジョブの site を束縛と照合する）で変わるからである。キューの定義を持つ jobs が、
// internal/worker と各ドメインパッケージの両方から既に import される共通の置き場になる。
func VerifySite(clients map[string]*mirakc.Client, jobSite, queue string) (*mirakc.Client, error) {
	client, ok := clients[jobSite]
	if !ok {
		return nil, fmt.Errorf(
			"%s: job site %q is not among this process's bound sites %s, refusing before touching mirakc/FS (issue #139)",
			queue, jobSite, sortedSiteNames(clients))
	}
	return client, nil
}

// sortedSiteNames は clients のキー（site 名）をソートして返す。VerifySite の
// エラーメッセージ用（map の反復順は不定なので、エラーメッセージが実行のたびに
// 揺れないようにする）。
func sortedSiteNames(clients map[string]*mirakc.Client) []string {
	sites := make([]string, 0, len(clients))
	for site := range clients {
		sites = append(sites, site)
	}
	sort.Strings(sites)
	return sites
}
