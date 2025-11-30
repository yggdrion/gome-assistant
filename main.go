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
	StandbyStartTime *time.Time // When the printer entered standby mode
	LastRelayOnTime  *time.Time // When we last detected relay was turned on
	WasOffline       bool       // Whether the printer was offline in the last check
	ShellyIP         string     // Cached Shelly device IP from metrics
}

// VMQueryResult represents a VictoriaMetrics query result
type VMQueryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
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

	// First check if relay is on (power > 0 means relay is on)
	watts, shellyIP, err := getShellyBambuWatts(cfg)
	if err != nil {
		log.Printf("Error getting shelly watts: %v", err)
		// If we can't get watts, printer might be offline (relay off)
		if !state.WasOffline {
			log.Println("Printer appears to be offline (can't read shelly watts)")
			state.WasOffline = true
			state.StandbyStartTime = nil
		}
		return
	}

	// Cache the Shelly IP for relay control
	if shellyIP != "" {
		state.ShellyIP = shellyIP
	}

	// Detect if printer was just turned on (was offline, now has power)
	if state.WasOffline && watts > 0 {
		now := time.Now()
		state.LastRelayOnTime = &now
		state.WasOffline = false
		state.StandbyStartTime = nil
		log.Printf("Printer was turned on, starting boot grace period of %s", cfg.BootGracePeriod)
		return
	}
	state.WasOffline = false

	// Check if we're still in boot grace period
	if state.LastRelayOnTime != nil {
		elapsed := time.Since(*state.LastRelayOnTime)
		if elapsed < cfg.BootGracePeriod {
			remaining := cfg.BootGracePeriod - elapsed
			log.Printf("Still in boot grace period, %.0f minutes remaining", remaining.Minutes())
			return
		}
	}

	// Check if any bambu printer is currently printing
	isPrinting, err := isBambuPrinting(cfg)
	if err != nil {
		log.Printf("Error checking bambu print status: %v", err)
		return
	}

	if isPrinting {
		log.Println("Printer is currently printing, resetting standby timer")
		state.StandbyStartTime = nil
		return
	}

	log.Printf("Printer idle, current power consumption: %.2f watts", watts)

	// Check if power is in the standby range (between min and max watts)
	inStandbyRange := watts > cfg.MinWatts && watts < cfg.MaxWatts

	if inStandbyRange {
		// Start or continue standby timer
		if state.StandbyStartTime == nil {
			now := time.Now()
			state.StandbyStartTime = &now
			log.Printf("Printer entered standby mode, starting %s timer", cfg.StandbyDuration)
			return
		}

		elapsed := time.Since(*state.StandbyStartTime)
		if elapsed >= cfg.StandbyDuration {
			log.Printf("Printer has been in standby for %s (threshold: %s), turning off relay", elapsed.Round(time.Second), cfg.StandbyDuration)
			if state.ShellyIP == "" {
				log.Printf("Error: No Shelly IP available")
			} else if err := setShellyRelayOff(cfg, state.ShellyIP); err != nil {
				log.Printf("Error turning off relay: %v", err)
			} else {
				log.Println("Relay turned off successfully")
				state.StandbyStartTime = nil
				state.WasOffline = true // Mark as offline since we just turned it off
			}
		} else {
			remaining := cfg.StandbyDuration - elapsed
			log.Printf("Printer in standby for %s, %.0f minutes until auto-off", elapsed.Round(time.Second), remaining.Minutes())
		}
	} else {
		// Power outside standby range, reset timer
		if state.StandbyStartTime != nil {
			log.Printf("Power consumption (%.2f W) left standby range, resetting timer", watts)
			state.StandbyStartTime = nil
		} else {
			log.Printf("Power consumption (%.2f W) is outside standby range (%.1f-%.1f W)", watts, cfg.MinWatts, cfg.MaxWatts)
		}
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
