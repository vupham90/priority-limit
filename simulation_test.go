package prioritylimit

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

func initPrometheus() {
	RegisterMetrics()
	http.Handle("/metrics", promhttp.Handler())

	// Start the HTTP server - bind to all interfaces
	go func() {
		if err := http.ListenAndServe("0.0.0.0:8080", nil); err != nil {
			fmt.Printf("Failed to start metrics server: %v\n", err)
		}
	}()
	time.Sleep(time.Second)
}

func TestSimulationScenarios(t *testing.T) {
	initPrometheus()
	redisAddr, cleanup := setupRedisContainer(t)
	defer cleanup()

	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	defer rdb.Close()

	// Define test scenarios
	scenarios := []struct {
		name           string
		config         SimulationConfig
		expectedMinReq int
	}{
		{
			name: "Ramping Load Pattern",
			config: SimulationConfig{
				// Basic rate limit settings
				WindowSize:      time.Second,
				TokensPerWindow: 100,

				// Base traffic rates
				HighPriorityBaseRPS:   100, // 50 requests per second base rate
				NormalPriorityBaseRPS: 20,  // 20 requests per second for normal priority

				// Load pattern configuration
				MinLoadPercent:   1,   // Start at 10% of base load
				MaxLoadPercent:   300, // Peak at 300% of base load
				RampUpDuration:   45,  // 5 seconds to ramp up
				RampDownDuration: 45,  // 5 seconds to ramp down

				// Use multiple workers to test borrowing behavior
				HighPriorityWorkers: 200,

				// Run for 30 seconds to see multiple cycles
				Duration: 600 * time.Second,
			},
			// Expected minimum requests calculation:
			// Cycle length = 10 seconds (5s up + 5s down)
			// Average load = (10% + 300%)/2 = 155% of base rate
			// High Priority: 50 RPS * 1.55 average multiplier = 77.5 RPS
			// Normal Priority: 20 RPS constant
			// Total for 30 seconds: (77.5 + 20) * 30 = 2925 minimum expected requests
			expectedMinReq: 2925,
		},
	}

	// Run the scenario
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			fmt.Printf("\nRunning scenario: %s\n", sc.name)
			fmt.Printf("Configuration:\n")
			fmt.Printf("  Window Size: %v\n", sc.config.WindowSize)
			fmt.Printf("  Tokens Per Window: %d\n", sc.config.TokensPerWindow)
			fmt.Printf("  High Priority Base RPS: %.1f\n", sc.config.HighPriorityBaseRPS)
			fmt.Printf("  Normal Priority Base RPS: %.1f\n", sc.config.NormalPriorityBaseRPS)
			fmt.Printf("  Load Pattern: %.1f%% to %.1f%%\n", sc.config.MinLoadPercent, sc.config.MaxLoadPercent)
			fmt.Printf("  Ramp Pattern: %.1fs up, %.1fs down\n", sc.config.RampUpDuration, sc.config.RampDownDuration)
			fmt.Printf("  High Priority Workers: %d\n", sc.config.HighPriorityWorkers)
			fmt.Printf("  Duration: %v\n", sc.config.Duration)

			ctx := context.Background()
			results := RunSimulation(ctx, rdb, sc.config)

			if len(results) < sc.expectedMinReq {
				t.Errorf("Expected at least %d requests, got %d",
					sc.expectedMinReq, len(results))
			}

			PrintSimulationResults(results)
		})
	}
}
