package prioritylimit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func init() {
	// Configure testcontainers to use Podman
	os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	os.Setenv("DOCKER_HOST", "unix:///run/podman/podman.sock")
}

func setupRedisContainer(t *testing.T) (string, func()) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "redis:latest",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections"),
	}

	redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatal(err)
	}

	mappedPort, err := redisC.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatal(err)
	}

	hostIP, err := redisC.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}

	redisAddr := hostIP + ":" + mappedPort.Port()

	cleanup := func() {
		if err := redisC.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate container: %s", err)
		}
	}

	return redisAddr, cleanup
}

func TestRateLimiter(t *testing.T) {
	redisAddr, cleanup := setupRedisContainer(t)
	defer cleanup()

	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	defer rdb.Close()

	limiter := NewPriorityLimiter(rdb)

	tests := []struct {
		name     string
		testFunc func(t *testing.T, limiter *PriorityLimiter)
	}{
		{"TestBasicRateLimit", testBasicRateLimit},
		{"TestPriorityAccess", testPriorityAccess},
		{"TestWindowRollover", testWindowRollover},
		{"TestHighPriorityWaiting", testHighPriorityWaiting},
		{"TestContextCancellation", testContextCancellation},
		{"TestParallelHighPriorityRequests", testParallelHighPriorityRequests},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.testFunc(t, limiter)
		})
	}
}

func testBasicRateLimit(t *testing.T, limiter *PriorityLimiter) {
	ctx := context.Background()
	cfg := Config{
		WindowSize: time.Second,
		Tokens:     2,
	}

	// First request should succeed
	allowed, err := limiter.Allow(ctx, "test1", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("First request should be allowed")
	}

	// Second request should succeed
	allowed, err = limiter.Allow(ctx, "test1", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("Second request should be allowed")
	}

	// Third request should fail
	allowed, err = limiter.Allow(ctx, "test1", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("Third request should be denied")
	}
}

func testPriorityAccess(t *testing.T, limiter *PriorityLimiter) {
	ctx := context.Background()
	cfg := Config{
		WindowSize: time.Second,
		Tokens:     1,
	}

	// Consume the current window
	allowed, err := limiter.Allow(ctx, "test2", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("First request should be allowed")
	}

	// Normal priority should be denied
	allowed, err = limiter.Allow(ctx, "test2", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("Normal priority should be denied when window is full")
	}

	// High priority should be allowed to use next window
	allowed, err = limiter.Allow(ctx, "test2", PriorityHigh, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("High priority should be allowed to use next window")
	}
}

func testWindowRollover(t *testing.T, limiter *PriorityLimiter) {
	ctx := context.Background()
	cfg := Config{
		WindowSize: time.Second,
		Tokens:     1,
	}

	// Consume current window
	allowed, err := limiter.Allow(ctx, "test3", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("First request should be allowed")
	}

	// Wait for window to roll over
	time.Sleep(time.Second)

	// Should be allowed in new window
	allowed, err = limiter.Allow(ctx, "test3", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("Request should be allowed in new window")
	}
}

func testHighPriorityWaiting(t *testing.T, limiter *PriorityLimiter) {
	ctx := context.Background()
	cfg := Config{
		WindowSize: 500 * time.Millisecond, // Smaller window for faster testing
		Tokens:     1,
	}

	// Consume the current window
	start := time.Now()
	allowed, err := limiter.Allow(ctx, "test-wait", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("First request should be allowed")
	}

	// High priority request should wait for next window
	allowed, err = limiter.Allow(ctx, "test-wait", PriorityHigh, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("High priority request should be allowed")
	}

	elapsed := time.Since(start)
	if elapsed < 450*time.Millisecond { // slightly less than window size to account for processing time
		t.Errorf("High priority request didn't wait long enough. Expected ~500ms, got %v", elapsed)
	}
}

func testContextCancellation(t *testing.T, limiter *PriorityLimiter) {
	cfg := Config{
		WindowSize: 500 * time.Millisecond,
		Tokens:     1,
	}

	// Consume the current window
	ctx := context.Background()
	allowed, err := limiter.Allow(ctx, "test-cancel", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("First request should be allowed")
	}

	// Create a context that will be cancelled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start a goroutine to cancel the context after a short delay
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	// Try high priority request with cancellable context
	allowed, err = limiter.Allow(ctx, "test-cancel", PriorityHigh, cfg)
	if err != context.Canceled {
		t.Errorf("Expected context.Canceled error, got %v", err)
	}
	if allowed {
		t.Error("Request should not be allowed after context cancellation")
	}
}

func testParallelHighPriorityRequests(t *testing.T, limiter *PriorityLimiter) {
	ctx := context.Background()
	cfg := Config{
		WindowSize: 500 * time.Millisecond,
		Tokens:     1,
	}

	// Consume the current window
	allowed, err := limiter.Allow(ctx, "test-parallel", PriorityNormal, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("First request should be allowed")
	}

	// Launch multiple high priority requests in parallel
	const numRequests = 3
	type result struct {
		allowed bool
		elapsed time.Duration
		err     error
	}
	results := make(chan result, numRequests)
	start := time.Now()

	for i := 0; i < numRequests; i++ {
		go func() {
			startReq := time.Now()
			allowed, err := limiter.Allow(ctx, "test-parallel", PriorityHigh, cfg)
			results <- result{
				allowed: allowed,
				elapsed: time.Since(startReq),
				err:     err,
			}
		}()
	}

	// Collect results
	var successCount int
	var deniedCount int
	for i := 0; i < numRequests; i++ {
		res := <-results
		if res.err != nil {
			t.Errorf("Unexpected error: %v", res.err)
			continue
		}

		if res.allowed {
			successCount++
			// Check if the successful request waited for approximately the window duration
			// Allow for some variance in timing (±50ms)
			minWait := 350 * time.Millisecond // window size - 150ms
			maxWait := 550 * time.Millisecond // window size + 50ms
			if res.elapsed < minWait || res.elapsed > maxWait {
				t.Errorf("Successful request wait time outside acceptable range [%v, %v]: got %v",
					minWait, maxWait, res.elapsed)
			}
		} else {
			deniedCount++
		}
	}

	// Only one request should succeed (get the token from next window)
	if successCount != 1 {
		t.Errorf("Expected exactly 1 successful request, got %d", successCount)
	}

	// The rest should be denied
	if deniedCount != numRequests-1 {
		t.Errorf("Expected %d denied requests, got %d", numRequests-1, deniedCount)
	}

	// Check total test duration with more lenient bounds
	totalElapsed := time.Since(start)
	minTestDuration := 350 * time.Millisecond
	if totalElapsed < minTestDuration {
		t.Errorf("Test completed too quickly. Expected at least %v, got %v",
			minTestDuration, totalElapsed)
	}
}
