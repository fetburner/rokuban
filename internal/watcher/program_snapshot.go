package watcher

import (
	"encoding/json"
	"time"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/ptr"
)

// programSnapshot は recordings に保存する番組・サービスの観測値をまとめる。
type programSnapshot struct {
	serviceName       string
	channelType       string
	channel           string
	title             string
	description       *string
	extended          json.RawMessage
	genres            json.RawMessage
	isFree            bool
	programStartAt    time.Time
	programDurationMs int64
}

// snapshotFromRecord は mirakc の Record から番組スナップショットを作る。
func snapshotFromRecord(record mirakc.Record) programSnapshot {
	return programSnapshot{
		serviceName:       record.Service.Name,
		channelType:       record.Service.Channel.Type,
		channel:           record.Service.Channel.Channel,
		title:             ptr.Deref(record.Program.Name),
		description:       record.Program.Description,
		extended:          marshalJSONOrNull(record.Program.Extended),
		genres:            marshalJSONOrNull(record.Program.Genres),
		isFree:            record.Program.IsFree,
		programStartAt:    millisToTime(record.Program.StartAt),
		programDurationMs: ptr.Deref(record.Program.Duration),
	}
}

// snapshotFromSchedule は mirakc の Schedule とサービス情報から番組スナップショットを作る。
func snapshotFromSchedule(schedule mirakc.Schedule, service mirakc.Service) programSnapshot {
	return programSnapshot{
		serviceName:       service.Name,
		channelType:       service.Channel.Type,
		channel:           service.Channel.Channel,
		title:             ptr.Deref(schedule.Program.Name),
		description:       schedule.Program.Description,
		extended:          marshalJSONOrNull(schedule.Program.Extended),
		genres:            marshalJSONOrNull(schedule.Program.Genres),
		isFree:            schedule.Program.IsFree,
		programStartAt:    millisToTime(schedule.Program.StartAt),
		programDurationMs: ptr.Deref(schedule.Program.Duration),
	}
}

// withCreateOrGetFailedRecordingParams は番組スナップショットを Params に写す。
func (s programSnapshot) withCreateOrGetFailedRecordingParams(params sqlcgen.CreateOrGetFailedRecordingParams) sqlcgen.CreateOrGetFailedRecordingParams {
	params.ServiceName = s.serviceName
	params.ChannelType = s.channelType
	params.Channel = s.channel
	params.Title = s.title
	params.Description = s.description
	params.Extended = s.extended
	params.Genres = s.genres
	params.IsFree = s.isFree
	params.ProgramStartAt = s.programStartAt
	params.ProgramDurationMs = s.programDurationMs
	return params
}
