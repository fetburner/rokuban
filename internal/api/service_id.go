package api

import (
	"fmt"

	"github.com/fetburner/rokuban/internal/mirakc"
)

// splitServiceIDs は `?service=` の合成 id（`Service.id`）を DB の 2 列
// （network_id / service_id）に分解する。不正な値があればエラーメッセージを
// 返す（空文字なら妥当）。
//
// **openapi.yaml の `minimum` / `maximum` は生成ハンドラでは強制されない。**
// oapi-codegen の束縛は型変換だけを行い、スキーマの数値制約は見ない（実測:
// `?service=0` / `?service=-1` / `?service=9007199254740991` がいずれも 200 で
// 素通りした）。無視・切り詰めせず 400 にする規約（docs/api/rest.md）を守るのは
// この関数の責務になる。
//
// **上限は下限より重い。** 範囲外の大きな id は int32 への変換で巻き戻り、
// 実在するチャンネルに化ける（mirakc.MaxServiceID のコメント参照）。0 件を
// 返すのではなく誤った行を返すので、必ず 400 で止める。
//
// 分解した組で述語を組むことで (network_id, service_id, ...) の複合インデックスが
// そのまま効く。合成 id を式で計算して比較するとインデックスが効かない。
//
// **`internal/mirakc` を import しても不変条件 1 には触れない** --- 使うのは
// 合成規則の純関数だけで、mirakc への問い合わせは一切しない。規則を api 側に
// 書き写すと片方だけ直して忘れる形になるので、権威を 1 つに保つ。
func splitServiceIDs(ids []int64) (networkIDs, serviceIDs []int32, message string) {
	for _, id := range ids {
		if id <= 0 || id > mirakc.MaxServiceID {
			return nil, nil, fmt.Sprintf("invalid service %d (want 1..%d)", id, mirakc.MaxServiceID)
		}
		networkID, serviceID := mirakc.SplitServiceID(id)
		networkIDs = append(networkIDs, int32(networkID))
		serviceIDs = append(serviceIDs, int32(serviceID))
	}
	return networkIDs, serviceIDs, ""
}
