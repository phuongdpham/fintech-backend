package domain

import (
	"time"

	"github.com/google/uuid"
)

type OutboxStatus string

const (
	OutboxStatusPending   OutboxStatus = "PENDING"
	OutboxStatusPublished OutboxStatus = "PUBLISHED"
)

func (s OutboxStatus) Valid() bool {
	switch s {
	case OutboxStatusPending, OutboxStatusPublished:
		return true
	}
	return false
}

// AggregateType identifies which domain aggregate produced an outbox event.
// Kept as a string (not enum) so adapters can introduce new aggregate types
// without a domain-package change; emit a known-set check in the consumer.
type AggregateType string

const (
	AggregateTypeTransaction AggregateType = "Transaction"
	AggregateTypeAccount     AggregateType = "Account"
)

// OutboxEvent is the row written atomically alongside ledger mutations and
// later relayed to Kafka by the Outbox Worker.
//
// Payload holds proto-serialized event bytes (BYTEA in Postgres). EventSchema
// names the proto message (e.g. "fintech.ledger.v1.TransferCommittedV1") so
// consumers can route without parsing payload first. Payload is opaque to
// the domain — the producer chooses the schema; the worker forwards bytes
// to Kafka without re-encoding.
type OutboxEvent struct {
	ID            uuid.UUID
	AggregateType AggregateType
	AggregateID   uuid.UUID
	EventSchema   string
	Payload       []byte
	Status        OutboxStatus
	CreatedAt     time.Time
}

func NewOutboxEvent(id, aggregateID uuid.UUID, aggType AggregateType, schema string, payload []byte, createdAt time.Time) (*OutboxEvent, error) {
	if aggType == "" {
		return nil, ErrInvalidOutboxStatus
	}
	if len(payload) == 0 {
		return nil, ErrInvalidOutboxStatus
	}
	return &OutboxEvent{
		ID:            id,
		AggregateType: aggType,
		AggregateID:   aggregateID,
		EventSchema:   schema,
		Payload:       payload,
		Status:        OutboxStatusPending,
		CreatedAt:     createdAt,
	}, nil
}
