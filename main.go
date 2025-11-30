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
)

// Config holds the configuration for the assistant
type Config struct {
	VictoriaMetricsURL      string
	VictoriaMetricsUser     string
	VictoriaMetricsPassword string
	ShellyDeviceIP          string
	CheckInterval           time.Duration
	MinWatts                float64
	MaxWatts                float64
	DryRun                  bool
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
	cfg := Config{}

	flag.StringVar(&cfg.VictoriaMetricsURL, "vm-url", getEnv("VM_URL", "https://vm.r4b2.de"), "VictoriaMetrics URL")
	flag.StringVar(&cfg.VictoriaMetricsUser, "vm-user", getEnv("VM_USER", "admin"), "VictoriaMetrics basic auth user")
	flag.StringVar(&cfg.VictoriaMetricsPassword, "vm-password", getEnv("VM_PASSWORD", ""), "VictoriaMetrics basic auth password")
	flag.StringVar(&cfg.ShellyDeviceIP, "shelly-ip", getEnv("SHELLY_IP", ""), "Shelly device IP address for relay control")
	flag.DurationVar(&cfg.CheckInterval, "interval", parseDuration(getEnv("CHECK_INTERVAL", "60s")), "Check interval")
	flag.Float64Var(&cfg.MinWatts, "min-watts", parseFloat(getEnv("MIN_WATTS", "7")), "Minimum watts threshold")
	flag.Float64Var(&cfg.MaxWatts, "max-watts", parseFloat(getEnv("MAX_WATTS", "9")), "Maximum watts threshold")
	flag.BoolVar(&cfg.DryRun, "dry-run", getEnv("DRY_RUN", "false") == "true", "Dry run mode (don't actually switch relay)")
	flag.Parse()

	if cfg.VictoriaMetricsPassword == "" {
		log.Fatal("VM_PASSWORD is required")
	}
	if cfg.ShellyDeviceIP == "" {
		log.Fatal("SHELLY_IP is required")
	}

	log.Printf("Starting gome-assistant")
	log.Printf("VictoriaMetrics URL: %s", cfg.VictoriaMetricsURL)
	log.Printf("Shelly Device IP: %s", cfg.ShellyDeviceIP)
	log.Printf("Check interval: %s", cfg.CheckInterval)
	log.Printf("Watts threshold: %.1f - %.1f", cfg.MinWatts, cfg.MaxWatts)
	log.Printf("Dry run: %v", cfg.DryRun)

	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	// Run immediately on start
	checkAndControl(&cfg)

	for range ticker.C {
		checkAndControl(&cfg)
	}
}

func checkAndControl(cfg *Config) {
	log.Println("Checking printer and power status...")

	// Check if any bambu printer is currently printing
	isPrinting, err := isBambuPrinting(cfg)
	if err != nil {
		log.Printf("Error checking bambu print status: %v", err)
		return
	}

	if isPrinting {
		log.Println("Printer is currently printing, skipping power check")
		return
	}

	// Get shelly power consumption for bambu device
	watts, err := getShellyBambuWatts(cfg)
	if err != nil {
		log.Printf("Error getting shelly watts: %v", err)
		return
	}

	log.Printf("Printer idle, current power consumption: %.2f watts", watts)

	// Check if power is in the standby range (between min and max watts)
	if watts > cfg.MinWatts && watts < cfg.MaxWatts {
		log.Printf("Power consumption (%.2f W) is in standby range (%.1f - %.1f W), turning off relay", watts, cfg.MinWatts, cfg.MaxWatts)
		if err := setShellyRelayOff(cfg); err != nil {
			log.Printf("Error turning off relay: %v", err)
		} else {
			log.Println("Relay turned off successfully")
		}
	} else {
		log.Printf("Power consumption (%.2f W) is outside standby range, no action needed", watts)
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

// getShellyBambuWatts gets the power consumption of the shelly device connected to bambu
func getShellyBambuWatts(cfg *Config) (float64, error) {
	// Query for shelly device with "bambu" in the name (case insensitive matching in metric labels)
	query := `shelly_watts{device_name=~".*[Bb]ambu.*"}`
	result, err := queryVM(cfg, query)
	if err != nil {
		return 0, err
	}

	if len(result.Data.Result) == 0 {
		return 0, fmt.Errorf("no shelly device with 'bambu' in name found")
	}

	// Get the first matching device's power consumption
	if len(result.Data.Result[0].Value) >= 2 {
		valueStr, ok := result.Data.Result[0].Value[1].(string)
		if ok {
			var watts float64
			fmt.Sscanf(valueStr, "%f", &watts)
			return watts, nil
		}
	}

	return 0, fmt.Errorf("could not parse power value")
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
func setShellyRelayOff(cfg *Config) error {
	if cfg.DryRun {
		log.Println("[DRY RUN] Would turn off relay")
		return nil
	}

	// Shelly Gen1 API endpoint to turn off relay
	relayURL := fmt.Sprintf("http://%s/relay/0?turn=off", cfg.ShellyDeviceIP)

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
