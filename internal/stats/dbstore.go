package stats

import "github.com/OpenListTeam/OpenList/v4/internal/db"

// DBStore keeps the daily totals in the application database.
type DBStore struct{}

func (DBStore) AddDaily(day string, kind Kind, delta int64) error {
	return db.AddTrafficDaily(day, string(kind), delta)
}

func (DBStore) Sums(from, to string) (map[string]int64, error) { return db.TrafficSums(from, to) }

func (DBStore) Since(from string) ([]DayRow, error) {
	rows, err := db.TrafficDailySince(from)
	if err != nil {
		return nil, err
	}
	out := make([]DayRow, len(rows))
	for i, r := range rows {
		out[i] = DayRow{Day: r.Day, Kind: r.Kind, Bytes: r.Bytes}
	}
	return out, nil
}
