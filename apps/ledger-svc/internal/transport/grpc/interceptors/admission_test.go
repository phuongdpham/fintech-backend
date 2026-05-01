package interceptors_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/transport/grpc/interceptors"
)

// TestAdmission_AcceptUnderCap proves the interceptor passes through
// when in-flight count is below MaxInFlight. Single-shot baseline.
func TestAdmission_AcceptUnderCap(t *testing.T) {
	intc := interceptors.Admission(interceptors.AdmissionConfig{MaxInFlight: 10})
	info := &gogrpc.UnaryServerInfo{FullMethod: "/x/M"}

	out, err := intc(context.Background(), "req", info, passHandler)
	require.NoError(t, err)
	require.Equal(t, "req", out)
}

// TestAdmission_RejectAtCap is the load-bearing case: when MaxInFlight
// is reached, the next call rejects immediately with RESOURCE_EXHAUSTED.
// "Immediately" means without queueing — assert latency stays sub-ms.
func TestAdmission_RejectAtCap(t *testing.T) {
	intc := interceptors.Admission(interceptors.AdmissionConfig{MaxInFlight: 2})
	info := &gogrpc.UnaryServerInfo{FullMethod: "/x/M"}

	hold := make(chan struct{})
	slow := func(_ context.Context, req any) (any, error) {
		<-hold
		return req, nil
	}

	// Fill the two slots; spin a barrier until the in-flight counter
	// is at capacity. Without this, the third call can race and sneak
	// in before the first two claim their slots.
	var claimed atomic.Int64
	wrapped := func(ctx context.Context, req any) (any, error) {
		claimed.Add(1)
		out, err := slow(ctx, req)
		return out, err
	}
	for range 2 {
		go func() { _, _ = intc(context.Background(), "in-flight", info, wrapped) }()
	}
	for claimed.Load() < 2 {
		time.Sleep(time.Millisecond)
	}

	t0 := time.Now()
	_, err := intc(context.Background(), "third", info, passHandler)
	dt := time.Since(t0)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, st.Code())
	require.Less(t, dt, 5*time.Millisecond,
		"admission should reject in <1ms — never queue past the cap")

	close(hold)
}

// TestAdmission_DisabledWhenMaxZero confirms the no-op path. Lets
// tests / specific environments construct a server without admission
// without writing a special-cased ServerConfig path.
func TestAdmission_DisabledWhenMaxZero(t *testing.T) {
	intc := interceptors.Admission(interceptors.AdmissionConfig{MaxInFlight: 0})
	info := &gogrpc.UnaryServerInfo{FullMethod: "/x/M"}

	// Fire 100 concurrent calls; all should succeed.
	var calls atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := intc(context.Background(), "req", info, func(_ context.Context, req any) (any, error) {
				calls.Add(1)
				return req, nil
			})
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	require.EqualValues(t, 100, calls.Load())
}
