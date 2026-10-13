package model

// TrafficDaily is the number of bytes this node relayed on one local calendar
// day, per kind (see internal/stats). Day is the server's local date.
type TrafficDaily struct {
	Day   string `json:"day" gorm:"primaryKey;size:10"`
	Kind  string `json:"kind" gorm:"primaryKey;size:32"`
	Bytes int64  `json:"bytes"`
}
