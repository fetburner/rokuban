// Command schemadump は、指定した空の DB に埋め込みの goose マイグレーションを全て適用する。
// 適用後のスキーマの書き出しは scripts/schemadump.sh が pg_dump で行う。
//
// rokuban migrate up を使わないのは、それが River のマイグレーションも適用し、ダンプに
// river_* の表と関数が入るから。River は独立に版管理しており、アプリのスキーマではない。
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/fetburner/rokuban/internal/db"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: schemadump <dsn>")
		os.Exit(2)
	}
	if err := db.MigrateUp(context.Background(), os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
