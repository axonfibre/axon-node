package database

import (
	hivedb "github.com/axonfibre/fibre.go/kvstore/database"
	"github.com/axonfibre/fibre.go/kvstore/mapdb"
	"github.com/iotaledger/hornet/v2/pkg/database"
	"github.com/iotaledger/hornet/v2/pkg/metrics"
)

func newMapDB(metrics *metrics.DatabaseMetrics) *database.Database {
	return database.New(
		"",
		mapdb.NewMapDB(),
		hivedb.EngineMapDB,
		metrics,
		database.NewEvents(),
		false,
		nil,
	)
}
