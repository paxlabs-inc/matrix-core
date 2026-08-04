package developer

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func ControlAPITestPool() *pgxpool.Pool { return developerPool }

func ControlAPITestNow() time.Time { return developerNow() }
