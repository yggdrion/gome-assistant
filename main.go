package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/joho/godotenv"
)

// Config holds the configuration for the assistant
type Config struct {
	VictoriaMetricsURL      string
	VictoriaMetricsUser     string
	VictoriaMetricsPassword string
	ShellyDevicePattern     string
	CheckInterval           time.Duration
	MinWatts                float64
	MaxWatts                float64
	StandbyDuration         time.Duration
	BootGracePeriod         time.Duration
	DryRun                  bool
}

// State tracks the current state of the assistant
type State struct {
	ShellyIP string // Cached Shelly device IP from metrics
}

// VMQueryResult represents a VictoriaMetrics query result
type VMQueryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`  // For instant queries: [timestamp, value]
			Values [][]interface{}   `json:"values"` // For range queries: [[timestamp, value], ...]
		} `json:"result"`
	} `json:"data"`
}

func main() {
	// Load .env file if it exists
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found or error loading it: %v", err)
	}

	cfg := Config{}

	flag.StringVar(&cfg.VictoriaMetricsURL, "vm-url", getEnv("VM_URL", "https://vm.r4b2.de"), "VictoriaMetrics URL")
	flag.StringVar(&cfg.VictoriaMetricsUser, "vm-user", getEnv("VM_USER", "admin"), "VictoriaMetrics basic auth user")
	flag.StringVar(&cfg.VictoriaMetricsPassword, "vm-password", getEnv("VM_PASSWORD", ""), "VictoriaMetrics basic auth password")
	flag.StringVar(&cfg.ShellyDevicePattern, "shelly-pattern", getEnv("SHELLY_DEVICE_PATTERN", ".*[Bb]ambu.*"), "Regex pattern to match Shelly device name")
	flag.DurationVar(&cfg.CheckInterval, "interval", parseDuration(getEnv("CHECK_INTERVAL", "60s")), "Check interval")
	flag.Float64Var(&cfg.MinWatts, "min-watts", parseFloat(getEnv("MIN_WATTS", "7")), "Minimum watts threshold")
	flag.Float64Var(&cfg.MaxWatts, "max-watts", parseFloat(getEnv("MAX_WATTS", "9")), "Maximum watts threshold")
	flag.DurationVar(&cfg.StandbyDuration, "standby-duration", parseDuration(getEnv("STANDBY_DURATION", "15m")), "Duration printer must be in standby before turning off")
	flag.DurationVar(&cfg.BootGracePeriod, "boot-grace", parseDuration(getEnv("BOOT_GRACE_PERIOD", "20m")), "Grace period after printer is turned on before checking standby")
	flag.BoolVar(&cfg.DryRun, "dry-run", getEnv("DRY_RUN", "false") == "true", "Dry run mode (don't actually switch relay)")
	flag.Parse()

	if cfg.VictoriaMetricsPassword == "" {
		log.Fatal("VM_PASSWORD is required")
	}

	log.Printf("Starting gome-assistant")
	log.Printf("VictoriaMetrics URL: %s", cfg.VictoriaMetricsURL)
	log.Printf("Shelly Device Pattern: %s", cfg.ShellyDevicePattern)
	log.Printf("Check interval: %s", cfg.CheckInterval)
	log.Printf("Watts threshold: %.1f - %.1f", cfg.MinWatts, cfg.MaxWatts)
	log.Printf("Standby duration before off: %s", cfg.StandbyDuration)
	log.Printf("Boot grace period: %s", cfg.BootGracePeriod)
	log.Printf("Dry run: %v", cfg.DryRun)

	state := &State{}

	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	// Run immediately on start
	checkAndControl(&cfg, state)

	for range ticker.C {
		checkAndControl(&cfg, state)
	}
}

func checkAndControl(cfg *Config, state *State) {
	log.Println("Checking printer and power status...")

	// Get current shelly power consumption
	watts, shellyIP, err := getShellyBambuWatts(cfg)
	if err != nil {
		log.Printf("Error getting shelly watts: %v", err)
		return
	}

	// Cache the Shelly IP for relay control
	if shellyIP != "" {
		state.ShellyIP = shellyIP
	}

	// Safety check: Ensure we have metrics availability
	hasRecentMetrics, err := hasRecentShellyMetrics(cfg, cfg.CheckInterval*2)
	if err != nil || !hasRecentMetrics {
		log.Printf("WARNING: No recent Shelly metrics found, skipping relay control for safety")
		return
	}

	// Check if printer was recently turned on (relay went from off to on)
	// Look back BootGracePeriod + 1 minute to see power transitions
	powerOnRecently, err := wasPowerTurnedOnRecently(cfg, cfg.BootGracePeriod)
	if err != nil {
		log.Printf("Error checking power transition history: %v", err)
		return
	}

	if powerOnRecently {
		log.Printf("Printer was turned on within boot grace period (%s), skipping checks", cfg.BootGracePeriod)
		return
	}

	// Check if any bambu printer is currently printing or was printing recently
	isPrinting, err := isBambuPrinting(cfg)
	if err != nil {
		log.Printf("Error checking bambu print status: %v", err)
		return
	}

	if isPrinting {
		log.Println("Printer is currently printing, no action taken")
		return
	}

	// Check if printer was printing recently (within last 15 minutes for safety)
	wasPrintingRecently, err := wasPrintingRecently(cfg, 15*time.Minute)
	if err != nil {
		log.Printf("Error checking recent print history: %v", err)
		return
	}

	if wasPrintingRecently {
		log.Println("Printer was printing recently, waiting before checking standby")
		return
	}

	log.Printf("Printer idle, current power consumption: %.2f watts", watts)

	// Check if power has been in standby range for the required duration
	inStandbyRange := watts >= cfg.MinWatts && watts <= cfg.MaxWatts
	if !inStandbyRange {
		log.Printf("Power consumption (%.2f W) is outside standby range (%.1f-%.1f W)", watts, cfg.MinWatts, cfg.MaxWatts)
		return
	}

	// Query metrics to see how long power has been in standby range
	standbyDuration, err := getStandbyDuration(cfg, cfg.MinWatts, cfg.MaxWatts, cfg.StandbyDuration)
	if err != nil {
		log.Printf("Error checking standby duration: %v", err)
		return
	}

	if standbyDuration >= cfg.StandbyDuration {
		log.Printf("Printer has been in standby for %s (threshold: %s), turning off relay", standbyDuration.Round(time.Second), cfg.StandbyDuration)
		if state.ShellyIP == "" {
			log.Printf("Error: No Shelly IP available")
		} else if err := setShellyRelayOff(cfg, state.ShellyIP); err != nil {
			log.Printf("Error turning off relay: %v", err)
		} else {
			log.Println("Relay turned off successfully")
		}
	} else {
		remaining := cfg.StandbyDuration - standbyDuration
		log.Printf("Printer in standby for %s, %.0f minutes until auto-off", standbyDuration.Round(time.Second), remaining.Minutes())
	}
}

// isBambuPrinting checks if any bambu printer is currently printing
// bambulab_gcode_state: 0 = idle, 1 = running, 2 = paused, 3 = completed, 4 = error
func isBambuPrinting(cfg *Config) (bool, error) {
	query := `bambulab_gcode_state`
	result, err := queryVM(cfg, query)
	if err != nil {
		return false, err
	}

	for _, r := range result.Data.Result {
		if len(r.Value) >= 2 {
			valueStr, ok := r.Value[1].(string)
			if ok && (valueStr == "1" || valueStr == "2") {
				// 1 = running, 2 = paused (still consider paused as "printing")
				printer := r.Metric["printer"]
				log.Printf("Printer %s is printing/paused (state=%s)", printer, valueStr)
				return true, nil
			}
		}
	}

	return false, nil
}

// getShellyBambuWatts gets the power consumption and IP of the shelly device connected to bambu
func getShellyBambuWatts(cfg *Config) (float64, string, error) {
	// Query for shelly device matching the configured pattern
	query := fmt.Sprintf(`shelly_watts{device_name=~"%s"}`, cfg.ShellyDevicePattern)
	result, err := queryVM(cfg, query)
	if err != nil {
		return 0, "", err
	}

	if len(result.Data.Result) == 0 {
		return 0, "", fmt.Errorf("no shelly device matching pattern '%s' found", cfg.ShellyDevicePattern)
	}

	// Get the first matching device's power consumption and IP
	device := result.Data.Result[0]
	ipAddress := device.Metric["ip_address"]

	if len(device.Value) >= 2 {
		valueStr, ok := device.Value[1].(string)
		if ok {
			var watts float64
			fmt.Sscanf(valueStr, "%f", &watts)
			if ipAddress != "" {
				log.Printf("Found Shelly device at %s", ipAddress)
			}
			return watts, ipAddress, nil
		}
	}

	return 0, "", fmt.Errorf("could not parse power value")
}

// hasRecentShellyMetrics checks if shelly metrics have been updated recently
func hasRecentShellyMetrics(cfg *Config, within time.Duration) (bool, error) {
	query := fmt.Sprintf(`shelly_watts{device_name=~"%s"}`, cfg.ShellyDevicePattern)
	result, err := queryVM(cfg, query)
	if err != nil {
		return false, err
	}

	if len(result.Data.Result) == 0 {
		return false, nil
	}

	// Check if timestamp is recent
	if len(result.Data.Result[0].Value) >= 2 {
		timestampFloat, ok := result.Data.Result[0].Value[0].(float64)
		if ok {
			metricTime := time.Unix(int64(timestampFloat), 0)
			age := time.Since(metricTime)
			return age <= within, nil
		}
	}

	return false, nil
}

// wasPowerTurnedOnRecently checks if power went from 0 to >0 within the lookback period
func wasPowerTurnedOnRecently(cfg *Config, lookback time.Duration) (bool, error) {
	// Query for power transitions using range query
	query := fmt.Sprintf(`shelly_watts{device_name=~"%s"}`, cfg.ShellyDevicePattern)

	// Use range query to look back
	queryURL := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=60s",
		cfg.VictoriaMetricsURL,
		url.QueryEscape(query),
		time.Now().Add(-lookback-1*time.Minute).Unix(),
		time.Now().Unix())

	req, err := http.NewRequest("GET", queryURL, nil)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(cfg.VictoriaMetricsUser, cfg.VictoriaMetricsPassword)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("VM range query failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result VMQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}

	if len(result.Data.Result) == 0 {
		return false, nil
	}

	// Check for power transition from ~0 to >10W (indicating relay turned on)
	// This would indicate printer was powered on
	values := result.Data.Result[0].Values // Use Values for range query
	if len(values) > 2 {
		// Look for a transition from low to high power
		var previousLow bool
		for _, pair := range values {
			if len(pair) >= 2 {
				if valueStr, ok := pair[1].(string); ok {
					var watts float64
					fmt.Sscanf(valueStr, "%f", &watts)

					if watts < 5 {
						previousLow = true
					} else if previousLow && watts > 10 {
						// Found transition from off/low to on
						return true, nil
					}
				}
			}
		}
	}

	return false, nil
}

// wasPrintingRecently checks if the printer was printing within the lookback period
func wasPrintingRecently(cfg *Config, lookback time.Duration) (bool, error) {
	// Query for recent gcode_state values
	query := `max_over_time(bambulab_gcode_state[` + lookback.String() + `])`
	result, err := queryVM(cfg, query)
	if err != nil {
		return false, err
	}

	for _, r := range result.Data.Result {
		if len(r.Value) >= 2 {
			if valueStr, ok := r.Value[1].(string); ok {
				// If max state in the period was 1 or 2 (running/paused), it was printing
				if valueStr == "1" || valueStr == "2" {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// getStandbyDuration calculates how long power has been continuously in standby range
func getStandbyDuration(cfg *Config, minWatts, maxWatts float64, maxDuration time.Duration) (time.Duration, error) {
	// Query power values over the max duration + buffer
	lookback := maxDuration + 5*time.Minute
	query := fmt.Sprintf(`shelly_watts{device_name=~"%s"}`, cfg.ShellyDevicePattern)

	queryURL := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=60s",
		cfg.VictoriaMetricsURL,
		url.QueryEscape(query),
		time.Now().Add(-lookback).Unix(),
		time.Now().Unix())

	req, err := http.NewRequest("GET", queryURL, nil)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(cfg.VictoriaMetricsUser, cfg.VictoriaMetricsPassword)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("VM range query failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result VMQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	if len(result.Data.Result) == 0 {
		return 0, nil
	}

	// Find the continuous period where power was in standby range
	// Work backwards from most recent
	values := result.Data.Result[0].Values // Use Values for range query
	if len(values) > 0 {
		var standbyStart *time.Time

		// Iterate from newest to oldest
		for i := len(values) - 1; i >= 0; i-- {
			pair := values[i]
			if len(pair) >= 2 {
				timestampFloat, _ := pair[0].(float64)
				valueStr, _ := pair[1].(string)

				var watts float64
				fmt.Sscanf(valueStr, "%f", &watts)

				if watts > minWatts && watts < maxWatts {
					// Still in standby range
					t := time.Unix(int64(timestampFloat), 0)
					standbyStart = &t
				} else {
					// Left standby range, stop
					break
				}
			}
		}

		if standbyStart != nil {
			return time.Since(*standbyStart), nil
		}
	}

	return 0, nil
}

// queryVM queries VictoriaMetrics with the given PromQL query
func queryVM(cfg *Config, query string) (*VMQueryResult, error) {
	queryURL := fmt.Sprintf("%s/api/v1/query?query=%s", cfg.VictoriaMetricsURL, url.QueryEscape(query))

	req, err := http.NewRequest("GET", queryURL, nil)
	if err != nil {
		return nil, err
	}

	req.SetBasicAuth(cfg.VictoriaMetricsUser, cfg.VictoriaMetricsPassword)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("VM query failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result VMQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if result.Status != "success" {
		return nil, fmt.Errorf("VM query returned status: %s", result.Status)
	}

	return &result, nil
}

// setShellyRelayOff turns off the shelly relay
func setShellyRelayOff(cfg *Config, shellyIP string) error {
	if cfg.DryRun {
		log.Printf("[DRY RUN] Would turn off relay at %s", shellyIP)
		return nil
	}

	// Shelly Gen1 API endpoint to turn off relay
	relayURL := fmt.Sprintf("http://%s/relay/0?turn=off", shellyIP)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(relayURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("shelly relay command failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 60 * time.Second
	}
	return d
}

func parseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}
