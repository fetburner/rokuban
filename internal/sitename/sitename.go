// Package sitename は site 名の既定値を置く。
//
// 既定値を store に置くと、store を import しない jobs（キュー名の解決）が
// store に依存してしまう。値だけを持つ葉のパッケージに置き、store.DefaultSite は
// そこを指す別名にして、既存の利用者を動かさない。
package sitename

// Default は site が未指定のときに解決される既定のサイト名。
//
// 空文字列で参照された箇所（`--sites` 省略時の 0 サイト束縛や jobs.VerifySite の
// 空 site 引数）がこの値に解決される。mirakc を指すすべてのテーブルが site を持つので、
// 既定値の定義は 1 箇所に置く。
const Default = "default"
