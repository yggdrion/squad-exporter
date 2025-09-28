package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
)

// Server represents a server configuration
type Server struct {
	Name string `json:"Name"`
	URL  string `json:"Url"`
}

// BattleMetricsResponse represents the API response structure (simplified)
type BattleMetricsResponse struct {
	Data struct {
		Attributes struct {
			Name    string `json:"name"`
			Players int    `json:"players"`
			Details struct {
				Map           string `json:"map"`
				GameMode      string `json:"gameMode"`
				SquadPlayTime int    `json:"squad_playTime"`
				SquadTeamOne  string `json:"squad_teamOne"`
				SquadTeamTwo  string `json:"squad_teamTwo"`
			} `json:"details"`
		} `json:"attributes"`
	} `json:"data"`
}

// Prometheus metrics
var (
	// Main metrics with stable labels only
	fridaSquadPlayerCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "squad_player_count",
			Help: "Number of players on the squad server",
		},
		[]string{"server_short_name"},
	)

	fridaSquadPlayTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "squad_play_time_seconds",
			Help: "Current round play time in seconds",
		},
		[]string{"server_short_name"},
	)

	// Static server info (rarely changes)
	fridaSquadServerInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "squad_server_info",
			Help: "Static server information",
		},
		[]string{"server_short_name", "server_full_name"},
	)

	// Dynamic game state metrics
	fridaSquadCurrentMap = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "squad_current_map",
			Help: "Current map being played (value is always 1)",
		},
		[]string{"server_short_name", "map_name"},
	)

	fridaSquadCurrentGameMode = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "squad_current_game_mode",
			Help: "Current game mode (value is always 1)",
		},
		[]string{"server_short_name", "game_mode"},
	)

	fridaSquadCurrentTeams = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "squad_current_teams",
			Help: "Current team configuration (value is always 1)",
		},
		[]string{"server_short_name", "team_one", "team_two"},
	)

	scrapeErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "squad_server_scrape_errors_total",
			Help: "Total number of scrape errors",
		},
		[]string{"server_name"},
	)
)

// MetricsCollector handles collecting metrics from BattleMetrics API
type MetricsCollector struct {
	serversFile     string
	httpClient      *http.Client
	rateLimiter     *rate.Limiter
	lastServerCount int
	ticker          *time.Ticker
	rateLimitHits   int
	lastRateLimit   time.Time
}

// NewMetricsCollector creates a new metrics collector with rate limiting
func NewMetricsCollector(serversFile string) *MetricsCollector {
	// BattleMetrics limits: 60 requests/minute (1/sec), 15 requests/second burst
	// Optimize for maximum collection frequency while respecting limits
	// - Burst: 15 requests (allows collecting all servers quickly)
	// - Refill: 1 request/second (stays within 60/minute limit)
	rateLimiter := rate.NewLimiter(rate.Every(time.Second), 15)

	return &MetricsCollector{
		serversFile:     serversFile,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		rateLimiter:     rateLimiter,
		lastServerCount: -1, // Initialize to -1 to force interval calculation on first run
	}
}

// fetchServerData fetches data from BattleMetrics API for a single server
func (mc *MetricsCollector) fetchServerData(server Server) error {
	// Wait for rate limiter permission
	ctx := context.Background()
	if err := mc.rateLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limiter error for server %s: %w", server.Name, err)
	}

	resp, err := mc.httpClient.Get(server.URL)
	if err != nil {
		scrapeErrors.WithLabelValues(server.Name).Inc()
		return fmt.Errorf("failed to fetch data for server %s: %w", server.Name, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Failed to close response body for server %s: %v", server.Name, closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		scrapeErrors.WithLabelValues(server.Name).Inc()

		// Handle rate limiting specifically
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			// Read rate limit headers if available
			retryAfter := resp.Header.Get("Retry-After")
			rateLimitRemaining := resp.Header.Get("X-RateLimit-Remaining")

			log.Printf("⚠️  Rate limit hit for server %s (HTTP %d). Retry-After: %s, Remaining: %s",
				server.Name, resp.StatusCode, retryAfter, rateLimitRemaining)

			// Return specific rate limit error
			return fmt.Errorf("rate limit exceeded for server %s (HTTP %d), retry after: %s",
				server.Name, resp.StatusCode, retryAfter)
		}

		return fmt.Errorf("unexpected status code %d for server %s", resp.StatusCode, server.Name)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		scrapeErrors.WithLabelValues(server.Name).Inc()
		return fmt.Errorf("failed to read response body for server %s: %w", server.Name, err)
	}

	var bmResp BattleMetricsResponse
	if err := json.Unmarshal(body, &bmResp); err != nil {
		scrapeErrors.WithLabelValues(server.Name).Inc()
		return fmt.Errorf("failed to unmarshal response for server %s: %w", server.Name, err)
	}

	// Update metrics
	mc.updateMetrics(server.Name, bmResp)
	return nil
}

// updateMetrics updates Prometheus metrics with server data
func (mc *MetricsCollector) updateMetrics(serverName string, resp BattleMetricsResponse) {
	attrs := resp.Data.Attributes

	// Update main metrics with stable labels only
	fridaSquadPlayerCount.WithLabelValues(serverName).Set(float64(attrs.Players))
	fridaSquadPlayTime.WithLabelValues(serverName).Set(float64(attrs.Details.SquadPlayTime))

	// Update static server info (value is always 1)
	fridaSquadServerInfo.WithLabelValues(
		serverName, // server_short_name
		attrs.Name, // server_full_name
	).Set(1)

	// Clear previous dynamic state metrics for this server to prevent stale data
	fridaSquadCurrentMap.DeletePartialMatch(prometheus.Labels{"server_short_name": serverName})
	fridaSquadCurrentGameMode.DeletePartialMatch(prometheus.Labels{"server_short_name": serverName})
	fridaSquadCurrentTeams.DeletePartialMatch(prometheus.Labels{"server_short_name": serverName})

	// Update dynamic game state metrics
	fridaSquadCurrentMap.WithLabelValues(
		serverName,        // server_short_name
		attrs.Details.Map, // map_name
	).Set(1)

	fridaSquadCurrentGameMode.WithLabelValues(
		serverName,             // server_short_name
		attrs.Details.GameMode, // game_mode
	).Set(1)

	fridaSquadCurrentTeams.WithLabelValues(
		serverName,                 // server_short_name
		attrs.Details.SquadTeamOne, // team_one
		attrs.Details.SquadTeamTwo, // team_two
	).Set(1)
}

// collectMetrics fetches data for all servers with rate limiting
func (mc *MetricsCollector) collectMetrics() {
	startTime := time.Now()

	// Reload servers from file before each collection
	servers, err := loadServers(mc.serversFile)
	if err != nil {
		log.Printf("Failed to reload servers from %s: %v", mc.serversFile, err)
		return
	}

	// Check if server count changed and adjust interval if needed
	if len(servers) != mc.lastServerCount {
		newInterval := calculateOptimalInterval(len(servers))
		log.Printf("Server count changed: %d → %d, adjusting interval to %v",
			mc.lastServerCount, len(servers), newInterval)

		// Reset ticker with new interval
		if mc.ticker != nil {
			mc.ticker.Reset(newInterval)
		}
		mc.lastServerCount = len(servers)
	}

	log.Printf("Starting metrics collection for %d servers (reloaded from %s)", len(servers), mc.serversFile)

	// Check if we recently hit rate limits and should back off
	if time.Since(mc.lastRateLimit) < 30*time.Second && mc.rateLimitHits > 0 {
		log.Printf("🚨 Recently hit rate limits (%d times), backing off for 30s", mc.rateLimitHits)
		return
	}

	// Process servers sequentially to respect rate limits
	successCount := 0
	rateLimitCount := 0
	for _, server := range servers {
		if err := mc.fetchServerData(server); err != nil {
			// Check if this was a rate limit error
			if strings.Contains(err.Error(), "rate limit exceeded") {
				rateLimitCount++
				mc.rateLimitHits++
				mc.lastRateLimit = time.Now()

				log.Printf("🚨 Rate limit detected! Stopping collection early to prevent further violations")
				break // Stop processing more servers
			}
			log.Printf("Error fetching data for server %s: %v", server.Name, err)
		} else {
			successCount++
			// Reset rate limit counter on successful request
			if mc.rateLimitHits > 0 {
				mc.rateLimitHits = 0
				log.Printf("✅ Rate limit counter reset after successful request")
			}
		}
	}

	duration := time.Since(startTime)
	if rateLimitCount > 0 {
		log.Printf("⚠️  Collection completed: %d/%d servers successful, %d rate limits hit, in %v",
			successCount, len(servers), rateLimitCount, duration)
	} else {
		log.Printf("✅ Collection completed: %d/%d servers successful in %v", successCount, len(servers), duration)
	}
}

// startMetricsCollection starts a goroutine that periodically collects metrics
func (mc *MetricsCollector) startMetricsCollection(interval time.Duration) {
	mc.ticker = time.NewTicker(interval)
	go func() {
		// Collect metrics immediately on startup
		mc.collectMetrics()

		for range mc.ticker.C {
			mc.collectMetrics()
		}
	}()
}

func loadServers(filename string) ([]Server, error) {
	// Check if the file exists and is actually a file (not a directory)
	fileInfo, err := os.Stat(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("servers file '%s' does not exist - please create it", filename)
		}
		return nil, fmt.Errorf("failed to stat servers file '%s': %w", filename, err)
	}

	// Ensure it's actually a file, not a directory
	if fileInfo.IsDir() {
		return nil, fmt.Errorf("'%s' is a directory, not a file - please remove the directory and create a proper JSON file", filename)
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read servers file '%s': %w", filename, err)
	}

	var servers []Server
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, fmt.Errorf("failed to decode servers JSON from '%s': %w", filename, err)
	}

	return servers, nil
}

// calculateOptimalInterval calculates the best collection interval based on server count
// BattleMetrics: 15 req/sec burst, 1 req/sec refill (60/minute)
func calculateOptimalInterval(serverCount int) time.Duration {
	const burstLimit = 15

	if serverCount == 0 {
		return 60 * time.Second // Default fallback
	}

	// If we have fewer servers than burst limit, we can collect more frequently
	if serverCount <= burstLimit {
		// Time to refill tokens for next full collection
		tokensNeeded := serverCount
		refillTime := time.Duration(tokensNeeded) * time.Second

		// Add small buffer (2 seconds) for network delays and safety margin
		return refillTime + 2*time.Second
	}

	// If we have more servers than burst limit, we need multiple bursts
	// This is a more complex scenario - for now, use conservative timing
	burstsNeeded := (serverCount + burstLimit - 1) / burstLimit // Ceiling division
	totalTime := time.Duration(burstsNeeded*burstLimit) * time.Second

	// Add buffer for safety
	return totalTime + 5*time.Second
}

func main() {
	// Load server configurations initially to verify file format
	servers, err := loadServers("servers.json")
	if err != nil {
		log.Fatalf("FATAL: Cannot start without valid servers.json file: %v", err)
	}

	// Calculate optimal collection interval based on server count
	interval := calculateOptimalInterval(len(servers))

	log.Printf("Initial load: %d servers", len(servers))
	log.Printf("Rate limiting: 15 req/sec burst, 1 req/sec refill")
	log.Printf("Optimal collection interval: %v (based on %d servers)", interval, len(servers))
	log.Printf("Servers will be reloaded before each collection - interval may adjust dynamically")

	// Register Prometheus metrics
	prometheus.MustRegister(
		fridaSquadPlayerCount,
		fridaSquadPlayTime,
		fridaSquadServerInfo,
		fridaSquadCurrentMap,
		fridaSquadCurrentGameMode,
		fridaSquadCurrentTeams,
		scrapeErrors,
	)

	// Create metrics collector with filename
	collector := NewMetricsCollector("servers.json")

	// Start collecting metrics with dynamic interval
	collector.startMetricsCollection(interval)

	// Setup Chi router
	r := chi.NewRouter()
	r.Use(middleware.Logger, middleware.Recoverer, middleware.Heartbeat("/health"))
	r.Handle("/metrics", promhttp.Handler())
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Load current server count
		currentServers, err := loadServers("servers.json")
		serverCount := 0
		if err == nil {
			serverCount = len(currentServers)
		}

		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"service":   "Squad Server Metrics",
			"servers":   serverCount,
			"note":      "Servers are reloaded from servers.json before each collection",
			"endpoints": []string{"/metrics", "/health"},
		}); err != nil {
			log.Printf("Failed to encode JSON response: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	})

	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	log.Printf("Starting server on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}
