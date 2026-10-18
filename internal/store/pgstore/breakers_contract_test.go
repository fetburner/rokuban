package pgstore

import (
	"testing"

	"github.com/fetburner/rokuban/internal/store/contracttest"
	"github.com/fetburner/rokuban/internal/testutil"
)

func TestBreakersContract(t *testing.T) {
	contracttest.RunBreakers(t, func(t *testing.T) contracttest.BreakersFixture {
		pool := testutil.SetupDB(t)
		s := NewBreakers(pool)
		return contracttest.BreakersFixture{Breakers: s, Admin: s}
	})
}
