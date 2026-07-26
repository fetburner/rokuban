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
func ServiceID(networkID, serviceID int) int64 {
	return int64(networkID)*idMagicNumber + int64(serviceID)
}
