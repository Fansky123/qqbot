package model

import "time"

type Consultation struct {
	ID             string
	GroupID        string
	MessageID      string
	UserID         string
	ProjectID      string
	Question       string
	Reply          string
	LeaseToken     string
	LeaseExpiresAt time.Time
	CompletedAt    time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
