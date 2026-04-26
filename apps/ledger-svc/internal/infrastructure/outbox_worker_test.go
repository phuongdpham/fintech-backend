package infrastructure

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/domain"
	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/repository"
)

// ---------------------------------------------------------------------------
// Fakes — local to this test file. Kept minimal: just enough to exercise
// the drain → publish → mark cycle without spinning up Postgres or
// librdkafka.
// ---------------------------------------------------------------------------

type fakeOutboxStore struct {
	mu           sync.Mutex
	events       []*domain.OutboxEvent
	drainErr     error
	publishedIDs []uuid.UUID
}

func newFakeOutboxStore(events ...*domain.OutboxEvent) *fakeOutboxStore {
	return &fakeOutboxStore{events: events}
}

func (f *fakeOutboxStore) Drain(ctx context.Context, batchSize int, publish repository.DrainPublisher) (int, error) {
	f.mu.Lock()
	if f.drainErr != nil {
		err := f.drainErr
		f.mu.Unlock()
		return 0, err
	}
	take := f.events
	if len(take) > batchSize {
		take = take[:batchSize]
	}
	// Snapshot the input outside the lock so we don't hold it across publish.
	batch := make([]*domain.OutboxEvent, len(take))
	copy(batch, take)
	f.mu.Unlock()

	succeeded, err := publish(ctx, batch)
	if err != nil {
		return 0, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	succSet := make(map[uuid.UUID]struct{}, len(succeeded))
	for _, id := range succeeded {
		succSet[id] = struct{}{}
		f.publishedIDs = append(f.publishedIDs, id)
	}
	keep := f.events[:0]
	for _, e := range f.events {
		if _, ok := succSet[e.ID]; !ok {
			keep = append(keep, e)
		}
	}
	f.events = keep
	return len(succeeded), nil
}

func (f *fakeOutboxStore) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func (f *fakeOutboxStore) publishedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.publishedIDs)
}

type fakeProducer struct {
	mu              sync.Mutex
	produceErr      error
	failIndexes     map[int]struct{}
	produceCalls    int
	lastBatchSize   int
	lastTopic       string
	lastKeys        [][]byte
	lastHeaderKeys  []string // first message's header keys, for quick assertions
}

func (f *fakeProducer) ProduceBatch(_ context.Context, msgs []KafkaMessage) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.produceCalls++
	f.lastBatchSize = len(msgs)
	if len(msgs) > 0 {
		f.lastTopic = msgs[0].Topic
		f.lastKeys = make([][]byte, len(msgs))
		for i, m := range msgs {
			f.lastKeys[i] = m.Key
		}
		f.lastHeaderKeys = nil
		for _, h := range msgs[0].Headers {
			f.lastHeaderKeys = append(f.lastHeaderKeys, h.Key)
		}
	}
	if f.produceErr != nil {
		return nil, f.produceErr
	}
	out := make([]int, 0, len(msgs))
	for i := range msgs {
		if _, fail := f.failIndexes[i]; fail {
			continue
		}
		out = append(out, i)
	}
	return out, nil
}

func (f *fakeProducer) Close() error { return nil }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mkEvent() *domain.OutboxEvent {
	return &domain.OutboxEvent{
		ID:            uuid.New(),
		AggregateType: domain.AggregateTypeTransaction,
		AggregateID:   uuid.New(),
		Payload:       []byte(`{"k":"v"}`),
		Status:        domain.OutboxStatusPending,
		CreatedAt:     time.Now(),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestOutboxWorker_Tick(t *testing.T) {
	cases := []struct {
		name           string
		eventCount     int
		producerSetup  func(*fakeProducer)
		storeSetup     func(*fakeOutboxStore)
		wantDrained    int
		wantPending    int
		wantProduceErr bool
	}{
		{
			name:        "all messages produce-succeed → all marked PUBLISHED",
			eventCount:  5,
			wantDrained: 5,
			wantPending: 0,
		},
		{
			name:       "partial success → only succeeded marked PUBLISHED, rest stay PENDING",
			eventCount: 5,
			producerSetup: func(p *fakeProducer) {
				p.failIndexes = map[int]struct{}{1: {}, 3: {}}
			},
			wantDrained: 3,
			wantPending: 2,
		},
		{
			name:       "producer fundamental fault → entire batch rolls back to PENDING",
			eventCount: 5,
			producerSetup: func(p *fakeProducer) {
				p.produceErr = errors.New("kafka: queue full")
			},
			wantDrained:    0,
			wantPending:    5,
			wantProduceErr: true,
		},
		{
			name:        "empty outbox → no produce call, drained==0",
			eventCount:  0,
			wantDrained: 0,
			wantPending: 0,
		},
		{
			name:       "store error surfaces upward",
			eventCount: 3,
			storeSetup: func(s *fakeOutboxStore) {
				s.drainErr = errors.New("pg: down")
			},
			wantDrained:    0,
			wantPending:    3,
			wantProduceErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := make([]*domain.OutboxEvent, tc.eventCount)
			for i := range events {
				events[i] = mkEvent()
			}
			store := newFakeOutboxStore(events...)
			if tc.storeSetup != nil {
				tc.storeSetup(store)
			}
			producer := &fakeProducer{}
			if tc.producerSetup != nil {
				tc.producerSetup(producer)
			}

			w := NewOutboxWorker(store, producer, OutboxWorkerConfig{
				Topic:     "test.topic",
				BatchSize: 100,
			}, silentLog())

			drained, err := w.tick(context.Background())
			if tc.wantProduceErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantDrained, drained)
			require.Equal(t, tc.wantPending, store.pendingCount())
			require.Equal(t, tc.wantDrained, store.publishedCount())
		})
	}
}

func TestOutboxWorker_PublishBatch_BuildsExpectedMessage(t *testing.T) {
	store := newFakeOutboxStore(mkEvent())
	producer := &fakeProducer{}
	w := NewOutboxWorker(store, producer, OutboxWorkerConfig{
		Topic:     "fintech.test",
		BatchSize: 10,
	}, silentLog())

	_, err := w.tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, "fintech.test", producer.lastTopic)
	require.Equal(t, 1, producer.lastBatchSize)
	require.Contains(t, producer.lastHeaderKeys, "aggregate_type")
	require.Contains(t, producer.lastHeaderKeys, "outbox_event_id")
	require.NotEmpty(t, producer.lastKeys[0], "Kafka key should be aggregate_id for partition affinity")
}

// Run/Wait lifecycle: cancel context → all workers exit promptly.
func TestOutboxWorker_Run_HonorsContextCancellation(t *testing.T) {
	store := newFakeOutboxStore() // empty — workers will idle-sleep
	producer := &fakeProducer{}
	w := NewOutboxWorker(store, producer, OutboxWorkerConfig{
		Topic:          "t",
		BatchSize:      10,
		PollInterval:   10 * time.Millisecond,
		IdleInterval:   10 * time.Millisecond,
		BackoffOnError: 10 * time.Millisecond,
		MaxConcurrency: 3,
	}, silentLog())

	ctx, cancel := context.WithCancel(context.Background())
	w.Run(ctx)

	// Give the goroutines a moment to enter their loops.
	time.Sleep(20 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() {
		w.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("workers did not exit after ctx cancellation")
	}
}

// publishBatch with empty input is a no-op — guards against bookkeeping
// regressions where Drain hands an empty batch in but the producer is
// still called.
func TestOutboxWorker_PublishBatch_EmptyInputSkipsProducer(t *testing.T) {
	store := newFakeOutboxStore()
	producer := &fakeProducer{}
	w := NewOutboxWorker(store, producer, OutboxWorkerConfig{Topic: "t"}, silentLog())

	_, err := w.tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, producer.produceCalls, "producer must not be called for empty batches")
}
