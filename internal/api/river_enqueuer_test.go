package api

import (
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobq/riveradapter"
)

func legacyEnqueuerForTest(client *river.Client[pgx.Tx]) jobq.Enqueuer {
	return riveradapter.NewLegacyEnqueuer(client)
}
