package prioritylimit

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

func initPrometheus() {
	prometheus.MustRegister(requests)
	http.Handle("/metrics", promhttp.Handler())

	// Start the HTTP server
	go http.ListenAndServe(":8080", nil)
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
			name: "High Load Scenario",
			config: SimulationConfig{
				WindowSize:        time.Second,
				TokensPerWindow:   10,
				HighPriorityRPS:   15,
				NormalPriorityRPS: 15,
				Duration:          500 * time.Second,
			},
			expectedMinReq: 100, // (15+15 RPS * 5 seconds)
		},
		{
			name: "Normal Load Scenario",
			config: SimulationConfig{
				WindowSize:        time.Second,
				TokensPerWindow:   20,
				HighPriorityRPS:   5,
				NormalPriorityRPS: 5,
				Duration:          5 * time.Second,
			},
			expectedMinReq: 40,
		},
		{
			name: "High Priority Heavy",
			config: SimulationConfig{
				WindowSize:        time.Second,
				TokensPerWindow:   10,
				HighPriorityRPS:   20,
				NormalPriorityRPS: 5,
				Duration:          5 * time.Second,
			},
			expectedMinReq: 100,
		},
		{
			name: "High Priority Exceed Limit",
			config: SimulationConfig{
				WindowSize:        time.Second,
				TokensPerWindow:   10,
				HighPriorityRPS:   20,
				NormalPriorityRPS: 1,
				Duration:          5 * time.Second,
			},
			expectedMinReq: 100,
		},
		{
			name: "Burst Scenario",
			config: SimulationConfig{
				WindowSize:        time.Second * 2,
				TokensPerWindow:   30,
				HighPriorityRPS:   25,
				NormalPriorityRPS: 25,
				Duration:          4 * time.Second,
			},
			expectedMinReq: 150,
		},
	}

	// Run all scenarios
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			fmt.Printf("Running scenario: %s\n", sc.name)
			fmt.Printf("Configuration: %+v\n", sc.config)

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
