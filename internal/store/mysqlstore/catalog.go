package mysqlstore

import (
	"database/sql"

	"github.com/fetburner/rokuban/internal/store"
)

var _ store.Catalog = (*Catalog)(nil)

// Catalog は catalog の export / restore を MySQL で実装する。
// Export は 1 つの読み取り専用 REPEATABLE READ の tx から全表を読む。
// Restore は 1 tx で全表を書き、分類ルールの再評価と名前付きロックで直列化する。
type Catalog struct {
	db        *sql.DB
	siteNames []string
	storeName string
}

// NewCatalog は MySQL を使う Catalog を作る。siteNames は構成の全 site で、
// Restore が全 site の ruler 入力の版を進める。空の一覧は呼び出し側の配線の誤りなので
// panic する（黙って一部 site を仮定すると、古い評価がコミットされる）。
// storeName は catalog を書く・読む物理コピーの置き場の名前。置き場を持たないロールは "" を明示する。
func NewCatalog(db *sql.DB, siteNames []string, storeName string) *Catalog {
	if len(siteNames) == 0 {
		panic("mysqlstore.NewCatalog: siteNames must not be empty")
	}
	return &Catalog{db: db, siteNames: append([]string(nil), siteNames...), storeName: storeName}
}
