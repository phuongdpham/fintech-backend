package infrastructure

import (
	"context"
	"fmt"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
)

// KafkaMessage is the minimal driver-agnostic message envelope the
// outbox worker hands to a KafkaProducer. Topic, Key, and Value are
// the fields the worker actually populates today; Headers exists for
// trace-context propagation when telemetry lands.
type KafkaMessage struct {
	Topic   string
	Key     []byte
	Value   []byte
	Headers []KafkaHeader
}

type KafkaHeader struct {
	Key   string
	Value []byte
}

// KafkaProducer is the port the outbox worker depends on. Driver-agnostic
// by design — keeps the worker unit-testable without spinning up
// librdkafka, and lets us swap in a different transport (Pulsar, NATS, an
// HTTP relay) without touching worker code.
//
// ProduceBatch contract:
//   - returns successIdx: indexes into msgs that were successfully delivered.
//   - err is non-nil only on fundamental producer failure (queue full,
//     transport down). Per-message delivery failures are reported by
//     omission from successIdx.
//   - blocks until all delivery reports are received OR the context is
//     cancelled OR the configured produce timeout elapses.
type KafkaProducer interface {
	ProduceBatch(ctx context.Context, msgs []KafkaMessage) (successIdx []int, err error)
	Close() error
}

// ConfluentProducer adapts confluent-kafka-go's *kafka.Producer to the
// KafkaProducer port.
type ConfluentProducer struct {
	p              *kafka.Producer
	produceTimeout time.Duration
}

// ConfluentProducerConfig is the subset of librdkafka knobs we expose.
// Defaults match the plan's "exact-once event sourcing" intent:
//   - enable.idempotence=true: librdkafka deduplicates on the broker.
//   - acks=all: wait for full ISR replication before ACK.
//   - compression: snappy is the right CPU/network trade for JSON payloads.
//   - linger.ms: small batching window to coalesce produces under load.
type ConfluentProducerConfig struct {
	Brokers          string
	ClientID         string
	LingerMs         int
	CompressionType  string // snappy | lz4 | zstd | none
	ProduceTimeout   time.Duration
	AdditionalConfig map[string]any // escape hatch for ops tuning
}

func NewConfluentProducer(cfg ConfluentProducerConfig) (*ConfluentProducer, error) {
	if cfg.Brokers == "" {
		return nil, fmt.Errorf("kafka producer: bootstrap brokers required")
	}
	if cfg.LingerMs == 0 {
		cfg.LingerMs = 5
	}
	if cfg.CompressionType == "" {
		cfg.CompressionType = "snappy"
	}
	if cfg.ProduceTimeout == 0 {
		cfg.ProduceTimeout = 5 * time.Second
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "ledger-outbox"
	}

	conf := kafka.ConfigMap{
		"bootstrap.servers":  cfg.Brokers,
		"client.id":          cfg.ClientID,
		"enable.idempotence": true,
		"acks":               "all",
		"compression.type":   cfg.CompressionType,
		"linger.ms":          cfg.LingerMs,
		// Idempotent producer requires retries; librdkafka caps automatically.
		"retries":           10,
		"max.in.flight":     5,
		"delivery.timeout.ms": int(cfg.ProduceTimeout.Milliseconds()),
	}
	for k, v := range cfg.AdditionalConfig {
		conf[k] = v
	}

	p, err := kafka.NewProducer(&conf)
	if err != nil {
		return nil, fmt.Errorf("kafka producer: new: %w", err)
	}
	return &ConfluentProducer{p: p, produceTimeout: cfg.ProduceTimeout}, nil
}

// ProduceBatch enqueues every message with a per-call delivery channel,
// then drains the channel until every message has reported (success or
// failure) or the context is cancelled. We DO NOT use the producer's
// global Events() channel — sharing it across batches makes correlating
// per-batch outcomes painful and racy.
func (cp *ConfluentProducer) ProduceBatch(ctx context.Context, msgs []KafkaMessage) ([]int, error) {
	if len(msgs) == 0 {
		return nil, nil
	}

	deliveryCh := make(chan kafka.Event, len(msgs))
	enqueued := 0

	for i := range msgs {
		m := &msgs[i]
		km := &kafka.Message{
			TopicPartition: kafka.TopicPartition{
				Topic:     &m.Topic,
				Partition: kafka.PartitionAny,
			},
			Key:     m.Key,
			Value:   m.Value,
			Headers: toKafkaHeaders(m.Headers),
			Opaque:  i, // index in msgs; lets us match the delivery report back.
		}
		if err := cp.p.Produce(km, deliveryCh); err != nil {
			// Local-side queue rejection (queue full / fatal). Bail — the
			// worker will retry the whole batch on the next tick.
			return nil, fmt.Errorf("kafka producer: enqueue %d: %w", i, err)
		}
		enqueued++
	}

	timeout := time.After(cp.produceTimeout)
	successIdx := make([]int, 0, enqueued)
	for received := 0; received < enqueued; received++ {
		select {
		case <-ctx.Done():
			return successIdx, ctx.Err()
		case <-timeout:
			return successIdx, fmt.Errorf("kafka producer: delivery timeout after %s with %d/%d delivered",
				cp.produceTimeout, len(successIdx), enqueued)
		case ev := <-deliveryCh:
			km, ok := ev.(*kafka.Message)
			if !ok {
				// Other event types (errors, stats) aren't expected on a
				// per-call channel but skip defensively.
				received-- // don't count toward our enqueued total
				continue
			}
			idx, _ := km.Opaque.(int)
			if km.TopicPartition.Error != nil {
				// Per-message failure. Omit from successIdx; outbox row
				// stays PENDING and the next sweep will retry it.
				continue
			}
			successIdx = append(successIdx, idx)
		}
	}
	return successIdx, nil
}

func (cp *ConfluentProducer) Close() error {
	// Flush blocks for up to the timeout, then closes. 15s gives in-flight
	// batches a chance to drain on graceful shutdown without hanging boot.
	cp.p.Flush(15_000)
	cp.p.Close()
	return nil
}

func toKafkaHeaders(hs []KafkaHeader) []kafka.Header {
	if len(hs) == 0 {
		return nil
	}
	out := make([]kafka.Header, len(hs))
	for i, h := range hs {
		out[i] = kafka.Header{Key: h.Key, Value: h.Value}
	}
	return out
}
