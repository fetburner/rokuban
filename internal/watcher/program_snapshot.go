package watcher

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/fetburner/rokuban/internal/mirakc"
	"github.com/fetburner/rokuban/internal/ptr"
	"github.com/fetburner/rokuban/internal/store"
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

// recordingSnapshot は mirakc の識別値と番組スナップショットを store の契約型に写す。
func (s programSnapshot) recordingSnapshot(networkID, serviceID, eventID int) store.RecordingSnapshot {
	return store.RecordingSnapshot{
		NetworkID: int32(networkID), ServiceID: int32(serviceID), EventID: int32(eventID),
		ServiceName: s.serviceName, ChannelType: s.channelType, Channel: s.channel,
		Title: s.title, Description: s.description, Extended: s.extended, Genres: s.genres,
		IsFree: s.isFree, ProgramStartAt: s.programStartAt, ProgramDurationMs: s.programDurationMs,
	}
}

func recordObservation(site string, record mirakc.Record) (store.RecordObservation, error) {
	var failedReason json.RawMessage
	if record.Recording.FailedReason != nil {
		data, err := json.Marshal(record.Recording.FailedReason)
		if err != nil {
			return store.RecordObservation{}, fmt.Errorf("marshalling failed reason: %w", err)
		}
		failedReason = data
	}
	snapshot := snapshotFromRecord(record)
	// tag は programId しか運ばない（#53）。「自分が予約した record か」は
	// IsOurs（rokuban のタグ）で判定し、reservation の特定は record 自身が
	// 持つ Program.ID（tag のパースを経由しない、常に正確な値）で行う。
	return store.RecordObservation{
		Site: site, RecordID: record.ID, ProgramID: record.Program.ID,
		Snapshot: snapshot.recordingSnapshot(record.Program.NetworkID, record.Program.ServiceID, record.Program.EventID),
		Managed:  mirakc.IsOurs(record.Tags), Tags: record.Tags,
		Status: record.Recording.Status, StartedAt: millisToTimeNonNil(record.Recording.StartTime),
		EndedAt:     millisToTimePtr(record.Recording.EndTime),
		ContentPath: contentPathPtr(record.Content.Path), ContentLength: contentLengthPtr(record.Content.Length),
		FailedReason: failedReason,
	}, nil
}
