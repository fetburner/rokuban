// Package pgstoretest constructs pgstore implementations for tests outside the pgstore package.
package pgstoretest

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/store/pgstore"
)

// Analysis はテスト用の既定値で Analysis を作る。
func Analysis(pool *pgxpool.Pool) *pgstore.Analysis {
	return pgstore.NewAnalysis(pool)
}

// Breakers はテスト用の既定値で Breakers を作る。
func Breakers(pool *pgxpool.Pool) *pgstore.Breakers {
	return pgstore.NewBreakers(pool)
}

// Catalog はテスト用の既定値で Catalog を作る。sites を省略すると "default" だけにする。
func Catalog(pool *pgxpool.Pool, sites ...string) *pgstore.Catalog {
	if len(sites) == 0 {
		sites = []string{"default"}
	}
	return pgstore.NewCatalog(pool, sites)
}

// EPG はテスト用の既定値で EPG を作る。
func EPG(pool *pgxpool.Pool) *pgstore.EPG {
	return pgstore.NewEPG(pool)
}

// LabelRules はテスト用の既定値で LabelRules を作る。
func LabelRules(pool *pgxpool.Pool) *pgstore.LabelRules {
	return pgstore.NewLabelRules(pool)
}

// Media はテスト用の既定値で Media を作る。
func Media(pool *pgxpool.Pool) *pgstore.Media {
	return pgstore.NewMedia(pool)
}

// NeverScheduledEvents はテスト用の既定値で NeverScheduledEvents を作る。
func NeverScheduledEvents(pool *pgxpool.Pool) *pgstore.NeverScheduledEvents {
	return pgstore.NewNeverScheduledEvents(pool)
}

// Playback はテスト用の既定値で Playback を作る。
func Playback(pool *pgxpool.Pool) *pgstore.Playback {
	return pgstore.NewPlayback(pool)
}

// Recordings はテスト用の既定値で Recordings を作る。
func Recordings(pool *pgxpool.Pool) *pgstore.Recordings {
	return pgstore.NewRecordings(pool)
}

// Reservations はテスト用の既定値で Reservations を作る。
func Reservations(pool *pgxpool.Pool) *pgstore.Reservations {
	return pgstore.NewReservations(pool)
}

// Rules はテスト用の既定値で Rules を作る。sites を省略すると "default" だけにする。
func Rules(pool *pgxpool.Pool, sites ...string) *pgstore.Rules {
	if len(sites) == 0 {
		sites = []string{"default"}
	}
	return pgstore.NewRules(pool, sites)
}

// ScheduleSync はテスト用の既定値で ScheduleSync を作る。
func ScheduleSync(pool *pgxpool.Pool) *pgstore.ScheduleSync {
	return pgstore.NewScheduleSync(pool)
}

// Tuners はテスト用の既定値で Tuners を作る。
func Tuners(pool *pgxpool.Pool) *pgstore.Tuners {
	return pgstore.NewTuners(pool)
}
