package prioritylimit

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

var requests = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "rate_limit_request",
	},
	[]string{"request", "status"},
)

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
// It will automatically wait if a high priority request needs to use the next window
func (rl *PriorityLimiter) Allow(ctx context.Context, key string, priority string, cfg Config) (bool, error) {
	now := time.Now().UnixNano() / int64(time.Millisecond)

	// Execute Lua script
	result, err := rl.rdb.Eval(ctx, rateLimitScript, []string{key},
		cfg.Tokens,
		cfg.WindowSize.Milliseconds(),
		priority,
		now,
	).Slice()

	if err != nil {
		return false, err
	}

	allowed := result[0].(int64) == 1
	if !allowed {
		requests.WithLabelValues(priority, "rejected").Inc()
		return false, nil
	}

	windowTs := result[1].(int64)
	executeTime := time.Unix(0, windowTs*int64(time.Millisecond))

	// If we need to wait, create a timer and wait for either the context to be done
	// or the execute time to arrive
	if time.Now().Before(executeTime) {
		timer := time.NewTimer(time.Until(executeTime))
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			// Timer completed, continue with execution
		}
	}
	requests.WithLabelValues(priority, "success").Inc()
	return true, nil
}
