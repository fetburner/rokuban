package jobs

import (
	"testing"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/store"
)

// TestVerifySite は issue #532 の受け入れ基準 3 を固定する: VerifySite は
// clients（束縛サイトの map）に無い jobSite を拒み、ある jobSite は通して
// そのクライアントを返す。
//
// このテストが検出すべき変異: VerifySite が clients の中身を見ずに常に通す
// （map の判定 `clients[site]` を消して常に何らかのクライアントを返す等）。
// その場合 "unbound site is refused" が誤って通ってしまい、このテストが落ちる。
func TestVerifySite(t *testing.T) {
	t.Parallel()
	tokyoClient := mirakc.NewClient("http://tokyo.example", nil)
	takamatsuClient := mirakc.NewClient("http://takamatsu.example", nil)
	clients := map[string]*mirakc.Client{
		"tokyo":     tokyoClient,
		"takamatsu": takamatsuClient,
	}

	t.Run("bound site is allowed and returns its own client", func(t *testing.T) {
		got, err := VerifySite(clients, "tokyo", "ingest")
		if err != nil {
			t.Fatalf("VerifySite: %v", err)
		}
		if got != tokyoClient {
			t.Errorf("VerifySite returned %v, want the tokyo client", got)
		}
	})

	t.Run("a second bound site is also allowed (N-site binding)", func(t *testing.T) {
		got, err := VerifySite(clients, "takamatsu", "ingest")
		if err != nil {
			t.Fatalf("VerifySite: %v", err)
		}
		if got != takamatsuClient {
			t.Errorf("VerifySite returned %v, want the takamatsu client", got)
		}
	})

	t.Run("unbound site is refused", func(t *testing.T) {
		if _, err := VerifySite(clients, "osaka", "ingest"); err == nil {
			t.Fatal("VerifySite() error = nil, want error for a site not in clients")
		}
	})

	// jobSite は正規化しない: 空文字列は無条件に拒む。これは "default" という
	// 名前の site がたまたま束縛されているデプロイでも同じでなければならない
	// --- 正規化すると、args.Site 自体が壊れている（本来 VerifySite が拾うべき）
	// ジョブが「default site 宛のジョブ」として素通りしてしまう。
	t.Run("empty jobSite is refused even when a site named default is bound", func(t *testing.T) {
		defaultOnly := map[string]*mirakc.Client{store.DefaultSite: tokyoClient}
		if _, err := VerifySite(defaultOnly, "", "ingest"); err == nil {
			t.Fatal("VerifySite() error = nil, want error for an empty (unnormalized) jobSite")
		}
	})
}
