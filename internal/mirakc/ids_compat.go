package mirakc

import "github.com/fetburner/rokuban/internal/programid"

// The ID arithmetic belongs to programid, but these forwarding functions keep
// the mirakc package's existing internal surface source-compatible. New code
// should import programid directly so that identity arithmetic does not create
// a dependency on the mirakc client.
const MaxServiceID = programid.MaxServiceID

func SplitProgramID(programID int64) (networkID, serviceID, eventID int) {
	return programid.SplitProgramID(programID)
}

func ComposeProgramID(networkID, serviceID, eventID int) int64 {
	return programid.ComposeProgramID(networkID, serviceID, eventID)
}

func ServiceID(networkID, serviceID int) int64 {
	return programid.ServiceID(networkID, serviceID)
}

func SplitServiceID(id int64) (networkID, serviceID int) {
	return programid.SplitServiceID(id)
}
