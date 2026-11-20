// Package sitename は site 名の既定値を置く。
//
// 既定値が store にあると、キュー名を解決する jobs が store を import することになる。
// そのため値だけを持つ葉のパッケージに置く。store.DefaultSite はここを指す別名として残し、
// 既存の利用者を動かさない。
package sitename

// Default は site が空文字列のときに解決される既定のサイト名。
//
// site が空のとき、キュー名の解決（jobs.qualifyQueueName・jobs.PhysicalQueueFor の
// ゼロ値 Args）と api の sites 既定がこの値を使う。jobs.VerifySite は解決せず、
// 空の site を常に拒む。
const Default = "default"
