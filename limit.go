package prioritylimit

import (
	"context"
	"time"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

var (
	registrationOnce sync.Once
	requests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rate_limit_request",
			Help: "Counter of rate limit requests by priority and status",
		},
		[]string{"request", "status"},
	)

	windowBorrowHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rate_limit_window_borrow_duration_ms",
			Help:    "Time spent waiting for next window when borrowing (high priority requests only)",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000}, // millisecond buckets
		},
		[]string{"request"},
	)

	executionDurationHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rate_limit_execution_duration_ms",
			Help:    "Time spent executing rate limit check including any waiting",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000}, // millisecond buckets
		},
		[]string{"request", "status"},
	)

	redisLatencyHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rate_limit_redis_duration_ms",
			Help:    "Time spent executing Redis operations",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 25, 50, 100}, // millisecond buckets
		},
		[]string{"request"},
	)
)

func RegisterMetrics() {
	registrationOnce.Do(func() {
		prometheus.MustRegister(requests)
		prometheus.MustRegister(windowBorrowHistogram)
		prometheus.MustRegister(executionDurationHistogram)
		prometheus.MustRegister(redisLatencyHistogram)
	})
}

type PriorityLimiter struct {
	rdb *redis.Client
}

const (
	PriorityNormal = "normal"
	PriorityHigh   = "high"
)

type Config struct {
	WindowSize time.Duration
	Tokens     int64
}

func NewPriorityLimiter(redisClient *redis.Client) *PriorityLimiter {
	return &PriorityLimiter{
		rdb: redisClient,
	}
}

// rateLimitScript remains unchanged
const rateLimitScript = `
local key_prefix = KEYS[1]
local tokens = tonumber(ARGV[1])
local windowSize = tonumber(ARGV[2])
local isPriority = ARGV[3] == "high"
local now = tonumber(ARGV[4])

-- Calculate current and next window timestamps
local currentWindow = now - (now % windowSize)
local nextWindow = currentWindow + windowSize

-- Generate keys for current and next windows
local currentKey = key_prefix .. ":" .. currentWindow
local nextKey = key_prefix .. ":" .. nextWindow

-- Get current count
local currentCount = tonumber(redis.call('get', currentKey) or "0")

-- If within limit for current window
if currentCount < tokens then
    redis.call('incr', currentKey)
    -- Set expiry if key is new
    if currentCount == 0 then
        redis.call('pexpire', currentKey, windowSize)
    end
    return {1, currentWindow} -- Return success with current window
end

-- If high priority request, try to consume from next window
if isPriority then
    local nextCount = tonumber(redis.call('get', nextKey) or "0")
    if nextCount < tokens then
        redis.call('incr', nextKey)
        -- Set expiry if key is new
        if nextCount == 0 then
            redis.call('pexpire', nextKey, windowSize * 2)
        end
        return {1, nextWindow} -- Return success with next window timestamp
    end
end

return {0, 0} -- Return failure
`

// Allow checks if the request should be allowed based on rate limits
func (rl *PriorityLimiter) Allow(ctx context.Context, key string, priority string, cfg Config) (bool, error) {
	start := time.Now()
	defer func() {
		executionDurationHistogram.WithLabelValues(
			priority,
			"total",
		).Observe(float64(time.Since(start).Milliseconds()))
	}()

	now := time.Now().UnixNano() / int64(time.Millisecond)

	// Track Redis operation latency
	redisStart := time.Now()
	result, err := rl.rdb.Eval(ctx, rateLimitScript, []string{key},
		cfg.Tokens,
		cfg.WindowSize.Milliseconds(),
		priority,
		now,
	).Slice()
	redisLatencyHistogram.WithLabelValues(priority).Observe(float64(time.Since(redisStart).Milliseconds()))

	if err != nil {
		executionDurationHistogram.WithLabelValues(priority, "error").Observe(float64(time.Since(start).Milliseconds()))
		return false, err
	}

	allowed := result[0].(int64) == 1
	if !allowed {
		requests.WithLabelValues(priority, "rejected").Inc()
		executionDurationHistogram.WithLabelValues(priority, "rejected").Observe(float64(time.Since(start).Milliseconds()))
		return false, nil
	}

	windowTs := result[1].(int64)
	executeTime := time.Unix(0, windowTs*int64(time.Millisecond))

	// If we need to wait for the next window (high priority borrowing)
	if time.Now().Before(executeTime) {
		waitStart := time.Now()
		timer := time.NewTimer(time.Until(executeTime))
		defer timer.Stop()

		select {
		case <-ctx.Done():
			windowBorrowHistogram.WithLabelValues(priority).Observe(float64(time.Since(waitStart).Milliseconds()))
			executionDurationHistogram.WithLabelValues(priority, "context_cancelled").Observe(float64(time.Since(start).Milliseconds()))
			return false, ctx.Err()
		case <-timer.C:
			// Track how long we waited to borrow from next window
			windowBorrowHistogram.WithLabelValues(priority).Observe(float64(time.Since(waitStart).Milliseconds()))
		}
	}

	requests.WithLabelValues(priority, "success").Inc()
	executionDurationHistogram.WithLabelValues(priority, "success").Observe(float64(time.Since(start).Milliseconds()))
	return true, nil
}
