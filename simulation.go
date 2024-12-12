package prioritylimit

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type SimulationConfig struct {
	// Rate limit configuration
	WindowSize      time.Duration
	TokensPerWindow int64

	// Traffic generation rates (requests per second)
	HighPriorityRPS   float64
	NormalPriorityRPS float64

	// Duration to run simulation
	Duration time.Duration
}

type SimulationResult struct {
	Time     time.Time
	Priority string
	Allowed  bool
}

// generateRandomIntervals creates a slice of random intervals that sum up to the desired duration
func generateRandomIntervals(baseRPS float64, duration time.Duration) []time.Duration {
	baseInterval := float64(time.Second) / baseRPS
	totalTime := float64(duration)
	var intervals []time.Duration

	var accumulatedTime float64
	for accumulatedTime < totalTime {
		// Generate variation between -150% and +150%
		variation := (rand.Float64()*3 - 1.5) // generates number between -1.5 and 1.5
		interval := baseInterval * (1 + variation)

		// Ensure we don't generate impossibly small intervals
		if interval < baseInterval*0.1 {
			interval = baseInterval * 0.1
		}

		intervals = append(intervals, time.Duration(interval))
		accumulatedTime += interval
	}

	return intervals
}

func RunSimulation(ctx context.Context, rdb *redis.Client, cfg SimulationConfig) []SimulationResult {
	limiter := NewPriorityLimiter(rdb)
	results := make([]SimulationResult, 0)
	var mu sync.Mutex

	// Create wait group for our two processes
	var wg sync.WaitGroup
	wg.Add(2)

	// High priority traffic generator with pre-generated random intervals
	go func() {
		defer wg.Done()

		// Pre-generate all random intervals for the duration
		intervals := generateRandomIntervals(cfg.HighPriorityRPS, cfg.Duration)

		limitCfg := Config{
			WindowSize: cfg.WindowSize,
			Tokens:     cfg.TokensPerWindow,
		}

		start := time.Now()
		for _, interval := range intervals {
			select {
			case <-ctx.Done():
				return
			default:
				allowed, _ := limiter.Allow(ctx, "sim", PriorityHigh, limitCfg)
				now := time.Now()
				mu.Lock()
				results = append(results, SimulationResult{
					Time:     now,
					Priority: PriorityHigh,
					Allowed:  allowed,
				})
				mu.Unlock()

				// Sleep for the pre-generated random interval
				time.Sleep(interval)

				// Check if we've exceeded the duration
				if time.Since(start) >= cfg.Duration {
					break
				}
			}
		}
	}()

	// Normal priority traffic generator
	go func() {
		defer wg.Done()
		normalPriorityInterval := time.Duration(float64(time.Second) / cfg.NormalPriorityRPS)
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

// PrintSimulationResults prints a textual visualization of the simulation results
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
