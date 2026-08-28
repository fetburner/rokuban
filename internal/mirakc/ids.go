package mirakc

// idMagicNumber は Mirakurun/mirakc が service id・program id を合成・分解する際の基数。
// mirakc-core (src/models.rs) の MAGIC_NUMBER = 100_000 に対応する。
const idMagicNumber = 100000

// SplitProgramID は mirakc の program id を networkID・serviceID・eventID に分解する。
//
// mirakc の ProgramId::new / Mirakurun の ProgramItem.ts と同じ合成規則の逆変換であり、
// 以下の式に基づく。
//
//	networkID = programID / (idMagicNumber * idMagicNumber)
//	serviceID = (programID / idMagicNumber) % idMagicNumber
//	eventID   = programID % idMagicNumber
func SplitProgramID(programID int64) (networkID, serviceID, eventID int) {
	networkID = int(programID / (idMagicNumber * idMagicNumber))
	serviceID = int((programID / idMagicNumber) % idMagicNumber)
	eventID = int(programID % idMagicNumber)
	return networkID, serviceID, eventID
}

// ComposeProgramID は networkID・serviceID・eventID から mirakc の program id を合成する。
//
// mirakc の ProgramId::new / Mirakurun の ProgramItem.ts と同じ合成規則であり、
// 以下の式に基づく。
//
//	programID = networkID*idMagicNumber*idMagicNumber + serviceID*idMagicNumber + eventID
func ComposeProgramID(networkID, serviceID, eventID int) int64 {
	return int64(networkID)*idMagicNumber*idMagicNumber + int64(serviceID)*idMagicNumber + int64(eventID)
}

// ServiceID は networkID・serviceID から Mirakurun 互換の service id を合成する。
//
// mirakc の ServiceId::new / Mirakurun の ProgramItem.ts と同じ合成規則であり、
// 以下の式に基づく。
//
//	id = networkID*idMagicNumber + serviceID
//
// **この値が rokuban の API でもサービスの identity になる**（`Service.id`・
// `?service=`。openapi.yaml 参照）。SI の serviceID は network をまたぐと
// 一意でないため、絞り込み・選択・キャッシュキーには必ずこの合成 id を使う。
func ServiceID(networkID, serviceID int) int64 {
	return int64(networkID)*idMagicNumber + int64(serviceID)
}

// MaxServiceID は ServiceID が返しうる最大値。
//
// networkID / serviceID とも SI 上 16bit なので 65535*100000 + 65535。
// **API 境界でこの上限を検査しないと、範囲外の id が int32 への変換で巻き戻り、
// 実在するチャンネルの (network_id, service_id) に化ける**（`?service=429500003201024`
// は networkID 4295000032 → int32 32736 となり、正規の `?service=3273601024` と
// 同じ行を返す）。0 件になるより悪い --- 誤った入力が正しい応答に見える。
const MaxServiceID = 65535*idMagicNumber + 65535

// SplitServiceID は ServiceID の逆変換。合成 id を networkID・serviceID に戻す。
//
// DB は network_id / service_id を別々の列で持つ（合成 id は永続化しない）ので、
// API 境界で受けた id はここで分解してから述語に使う。分解した組で引けば既存の
// (network_id, service_id, ...) 複合インデックスがそのまま効く。
//
// 合成規則を 2 箇所に書き下さないため、ServiceID と対でここに置く。
func SplitServiceID(id int64) (networkID, serviceID int) {
	return int(id / idMagicNumber), int(id % idMagicNumber)
}
