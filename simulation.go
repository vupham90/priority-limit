package prioritylimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type SimulationConfig struct {
	// Rate limit configuration
	WindowSize      time.Duration
	TokensPerWindow int64

	// Base traffic rates (requests per second)
	HighPriorityBaseRPS   float64
	NormalPriorityBaseRPS float64

	// Pattern configuration
	MinLoadPercent  float64 // Minimum load (e.g., 10%)
	MaxLoadPercent  float64 // Maximum load (e.g., 300%)
	RampUpDuration  float64 // Time to go from min to max in seconds
	RampDownDuration float64 // Time to go from max to min in seconds

	// Number of concurrent workers for high priority requests
	HighPriorityWorkers int

	// Duration to run simulation
	Duration time.Duration
}

type SimulationResult struct {
	Time     time.Time
	Priority string
	Allowed  bool
}

// loadController manages deterministic load patterns
type loadController struct {
	startTime        time.Time
	minLoadPercent   float64
	maxLoadPercent   float64
	rampUpDuration   float64
	rampDownDuration float64
}

func newLoadController(minPercent, maxPercent, rampUpSec, rampDownSec float64) *loadController {
	return &loadController{
		startTime:        time.Now(),
		minLoadPercent:   minPercent,
		maxLoadPercent:   maxPercent,
		rampUpDuration:   rampUpSec,
		rampDownDuration: rampDownSec,
	}
}

func (lc *loadController) getCurrentMultiplier() float64 {
	cycleLength := lc.rampUpDuration + lc.rampDownDuration
	elapsed := time.Since(lc.startTime).Seconds()
	
	// Calculate position within the current cycle
	position := elapsed - (float64(int(elapsed/cycleLength)) * cycleLength)
	
	if position < lc.rampUpDuration {
		// During ramp up: linear interpolation from min to max
		progress := position / lc.rampUpDuration
		return lc.minLoadPercent/100.0 + (progress * ((lc.maxLoadPercent - lc.minLoadPercent)/100.0))
	} else {
		// During ramp down: linear interpolation from max to min
		progress := (position - lc.rampUpDuration) / lc.rampDownDuration
		return lc.maxLoadPercent/100.0 - (progress * ((lc.maxLoadPercent - lc.minLoadPercent)/100.0))
	}
}

func RunSimulation(ctx context.Context, rdb *redis.Client, cfg SimulationConfig) []SimulationResult {
	limiter := NewPriorityLimiter(rdb)
	var results []SimulationResult
	var mu sync.Mutex

	// Create load controller for synchronized patterns
	controller := newLoadController(
		cfg.MinLoadPercent,
		cfg.MaxLoadPercent,
		cfg.RampUpDuration,
		cfg.RampDownDuration,
	)

	// Create wait group for all workers
	var wg sync.WaitGroup
	wg.Add(cfg.HighPriorityWorkers + 1) // +1 for normal priority

	// Calculate base interval for each worker
	baseWorkerRPS := cfg.HighPriorityBaseRPS / float64(cfg.HighPriorityWorkers)

	// Launch high priority workers
	for i := 0; i < cfg.HighPriorityWorkers; i++ {
		go func(workerID int) {
			defer wg.Done()

			limitCfg := Config{
				WindowSize: cfg.WindowSize,
				Tokens:     cfg.TokensPerWindow,
			}

			start := time.Now()
			for time.Since(start) < cfg.Duration {
				// Get current multiplier from controller
				multiplier := controller.getCurrentMultiplier()
				
				// Calculate current interval based on multiplier
				currentRPS := baseWorkerRPS * multiplier
				interval := time.Duration(float64(time.Second) / currentRPS)

				select {
				case <-ctx.Done():
					return
				default:
					allowed, err := limiter.Allow(ctx, "sim", PriorityHigh, limitCfg)
					if err != nil {
						fmt.Printf("Worker %d error: %v\n", workerID, err)
						continue
					}

					mu.Lock()
					results = append(results, SimulationResult{
						Time:     time.Now(),
						Priority: PriorityHigh,
						Allowed:  allowed,
					})
					mu.Unlock()

					time.Sleep(interval)
				}
			}
		}(i)
	}

	// Normal priority traffic generator
	go func() {
		defer wg.Done()
		normalPriorityInterval := time.Duration(float64(time.Second) / cfg.NormalPriorityBaseRPS)
		ticker := time.NewTicker(normalPriorityInterval)
		defer ticker.Stop()

		limitCfg := Config{
			WindowSize: cfg.WindowSize,
			Tokens:     cfg.TokensPerWindow,
		}

		start := time.Now()
		for time.Since(start) < cfg.Duration {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				allowed, _ := limiter.Allow(ctx, "sim", PriorityNormal, limitCfg)
				mu.Lock()
				results = append(results, SimulationResult{
					Time:     t,
					Priority: PriorityNormal,
					Allowed:  allowed,
				})
				mu.Unlock()
			}
		}
	}()

	wg.Wait()
	return results
}

func PrintSimulationResults(results []SimulationResult) {
	if len(results) == 0 {
		return
	}

	startTime := results[0].Time

	// Group results by second
	bySecond := make(map[int]struct {
		high      int
		highRej   int
		normal    int
		normalRej int
		highTime  []time.Time // Track actual request times for high priority
	})

	for _, r := range results {
		second := int(r.Time.Sub(startTime).Seconds())
		stats := bySecond[second]

		if r.Priority == PriorityHigh {
			if r.Allowed {
				stats.high++
			} else {
				stats.highRej++
			}
			stats.highTime = append(stats.highTime, r.Time)
		} else {
			if r.Allowed {
				stats.normal++
			} else {
				stats.normalRej++
			}
		}

		bySecond[second] = stats
	}

	// Print results with additional timing information
	fmt.Println("\nSimulation Results (per second):")
	fmt.Println("Time\tHigh(✓/✗)\tNormal(✓/✗)\tHigh Priority Distribution")
	fmt.Println("--------------------------------------------------------------------------------")

	for i := 0; i <= len(bySecond); i++ {
		stats := bySecond[i]

		// Create a simple visualization of request distribution within the second
		distribution := make([]byte, 50)
		for j := range distribution {
			distribution[j] = '.'
		}
		for _, t := range stats.highTime {
			pos := int(t.Sub(startTime.Add(time.Duration(i)*time.Second)).Milliseconds() * 50 / 1000)
			if pos >= 0 && pos < 50 {
				distribution[pos] = '|'
			}
		}

		fmt.Printf("%ds\t%d/%d\t\t%d/%d\t\t%s\n",
			i,
			stats.high, stats.highRej,
			stats.normal, stats.normalRej,
			string(distribution),
		)
	}
}
