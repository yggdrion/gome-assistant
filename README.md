# gome-assistant

A helper service that monitors your Bambu Lab printer and automatically turns off its Shelly smart plug when the printer enters standby mode.

## What it does

1. Queries VictoriaMetrics for Bambu printer status (`bambulab_gcode_state`)
2. If no print is running, checks the power consumption of the Shelly device connected to the Bambu printer
3. If power consumption is between 7-9 watts (standby mode), turns off the Shelly relay

## Requirements

- VictoriaMetrics with bambulab-exporter and shelly-exporter metrics
- Shelly smart plug (Gen1) connected to your Bambu printer
- The Shelly device name must match the configured pattern (default: contains "bambu")
- The Shelly IP is automatically discovered from the `ip_address` label in metrics

## Configuration

Copy `.env.sample` to `.env` and configure:

```bash
cp .env.sample .env
```

| Variable                | Description                               | Default              |
| ----------------------- | ----------------------------------------- | -------------------- |
| `VM_URL`                | VictoriaMetrics URL                       | `https://vm.r4b2.de` |
| `VM_USER`               | Basic auth username                       | `admin`              |
| `VM_PASSWORD`           | Basic auth password                       | (required)           |
| `SHELLY_DEVICE_PATTERN` | Regex pattern to match Shelly device name | `.*[Bb]ambu.*`       |
| `CHECK_INTERVAL`        | How often to check                        | `60s`                |
| `MIN_WATTS`             | Minimum standby watts threshold           | `7`                  |
| `MAX_WATTS`             | Maximum standby watts threshold           | `9`                  |
| `STANDBY_DURATION`      | Time in standby before turning off        | `15m`                |
| `BOOT_GRACE_PERIOD`     | Grace period after printer turns on       | `20m`                |
| `DRY_RUN`               | Test mode without switching relay         | `false`              |

## Running

### Local

```bash
go build -o gome-assistant .
./gome-assistant
```

### Docker

```bash
docker build -t gome-assistant .
docker run --env-file .env gome-assistant
```

### Docker Compose

```bash
docker compose up -d
```

## Metrics used

- `bambulab_gcode_state` - Print state (0=idle, 1=running, 2=paused, 3=completed, 4=error)
- `shelly_watts{device_name=~".*[Bb]ambu.*"}` - Power consumption of Shelly device with "bambu" in name

## Logic

```
1. If printer was offline and is now on:
   - Start boot grace period (20 min default)
   - Don't check standby during this time

2. If still in boot grace period:
   - Skip all checks, let printer boot/start print

3. If printer is printing (gcode_state = 1 or 2):
   - Reset standby timer
   - Skip power check

4. If printer is idle AND power is in standby range (7-9W):
   - Start standby timer if not already running
   - If standby timer >= 15 minutes: Turn off relay

5. If power leaves standby range:
   - Reset standby timer
```
