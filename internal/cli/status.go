package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dappermint/ratatouille/internal/metrics"
	"github.com/dappermint/ratatouille/internal/storage"
	"github.com/dappermint/ratatouille/internal/text"
)

func runStatusCommand(ctx context.Context, terminalOut bool, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(errOut)
	jsonOutput := flags.Bool("json", false, "print machine-readable json")
	watch := flags.Bool("watch", false, "keep sampling until interrupted")
	interval := flags.Duration("interval", 2*time.Second, "how long to wait between samples in watch mode")
	explain := flags.Bool("explain", false, "show how the load score was computed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("status does not take arguments")
	}
	// Piped output is a script reading this, and a script wants json.
	if !terminalOut {
		*jsonOutput = true
	}
	tracker := metrics.NewTracker()

	if !*watch {
		snapshot := tracker.Observe(metrics.Collect(ctx))
		if *jsonOutput {
			return writeJSON(out, "status", snapshot)
		}
		printStatus(out, snapshot, *explain)
		return nil
	}

	// Watch mode emits one object per sample rather than an array, so a reader
	// can consume it a line at a time instead of waiting for an end that never
	// comes. On a terminal it repaints in place instead, because a dashboard
	// that scrolls is one you cannot read.
	encoder := json.NewEncoder(out)
	for {
		snapshot := tracker.Observe(metrics.Collect(ctx))
		switch {
		case *jsonOutput:
			if err := encoder.Encode(newJSONEnvelope("status", snapshot)); err != nil {
				return err
			}
		default:
			fmt.Fprint(out, clearScreen)
			printStatus(out, snapshot, *explain)
			fmt.Fprintf(out, "\nsampling every %s, ctrl-c to stop\n", *interval)
		}
		select {
		case <-ctx.Done():
			fmt.Fprint(out, showCursor)
			return nil
		case <-time.After(*interval):
		}
	}
}

// clearScreen homes the cursor and clears, then hides it so the caret does not
// blink over a repainting figure.
const (
	clearScreen = "\033[H\033[2J\033[?25l"
	showCursor  = "\033[?25h"
)

const jsonSchema = 2

type jsonEnvelope struct {
	Schema int    `json:"schema"`
	Kind   string `json:"kind"`
	Data   any    `json:"data"`
}

func newJSONEnvelope(kind string, value any) jsonEnvelope {
	return jsonEnvelope{Schema: jsonSchema, Kind: kind, Data: value}
}

func writeJSON(out io.Writer, kind string, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(newJSONEnvelope(kind, value))
}

func printStatus(out io.Writer, snapshot metrics.Snapshot, explain bool) {
	hardware := snapshot.Hardware
	fmt.Fprintf(out, "%s  load %d %s\n", snapshot.Host, snapshot.LoadScore, metrics.Level(snapshot.LoadScore))
	fmt.Fprintf(out, "%s · %s · %d cores · %s · macOS %s · up %s\n\n",
		hardware.Model, hardware.Chip, hardware.Cores,
		storage.HumanBytes(hardware.MemoryTotal), hardware.OS, uptime(hardware.Uptime))

	fmt.Fprintf(out, "%-9s %s  %5.1f%% busy   %.1f user  %.1f system\n",
		"cpu", meter(snapshot.CPU.Busy), snapshot.CPU.Busy, snapshot.CPU.User, snapshot.CPU.System)
	fmt.Fprintf(out, "%-9s load %.2f / %.2f / %.2f over %d cores\n",
		"", snapshot.CPU.Load[0], snapshot.CPU.Load[1], snapshot.CPU.Load[2], snapshot.CPU.Cores)
	if snapshot.GPU != nil {
		fmt.Fprintf(out, "%-9s %s  %5.1f%% device   %.1f renderer  %.1f tiler\n",
			"gpu", meter(snapshot.GPU.Device), snapshot.GPU.Device, snapshot.GPU.Renderer, snapshot.GPU.Tiler)
	}

	memory := snapshot.Memory
	fmt.Fprintf(out, "%-9s %s  %5.1f%% used   %s of %s\n",
		"memory", meter(memory.UsedShare), memory.UsedShare,
		storage.HumanBytes(memory.Used), storage.HumanBytes(memory.Total))
	fmt.Fprintf(out, "%-9s app %s  wired %s  compressed %s\n",
		"", storage.HumanBytes(memory.App), storage.HumanBytes(memory.Wired), storage.HumanBytes(memory.Compressed))
	fmt.Fprintf(out, "%-9s %s available, so pressure is %.1f%%\n",
		"", storage.HumanBytes(memory.Available), memory.Pressure)
	if memory.SwapTotal > 0 {
		fmt.Fprintf(out, "%-9s swap %s of %s\n", "",
			storage.HumanBytes(memory.SwapUsed), storage.HumanBytes(memory.SwapTotal))
	}

	if len(snapshot.Disks) > 0 {
		parts := make([]string, 0, len(snapshot.Disks))
		for _, disk := range snapshot.Disks {
			if disk.Throughput <= 0 && disk.Transfers <= 0 {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s %.1f MB/s", disk.Device, disk.Throughput))
		}
		if len(parts) == 0 {
			parts = append(parts, "idle")
		}
		fmt.Fprintf(out, "%-9s %s\n", "disk io", strings.Join(parts, "   "))
	}

	network := snapshot.Network
	if network.Interface != "" {
		fmt.Fprintf(out, "%-9s %s   down %s/s   up %s/s\n", "network", network.Interface,
			storage.HumanBytes(int64(network.DownRate)), storage.HumanBytes(int64(network.UpRate)))
	}

	if power := snapshot.Power; power != nil {
		line := fmt.Sprintf("%d%%  %s", power.Percent, power.State)
		if power.Remaining != "" {
			line += "  " + power.Remaining + " remaining"
		}
		fmt.Fprintf(out, "%-9s %s  %s\n", "power", meter(float64(power.Percent)), line)
		if power.Cycles > 0 {
			fmt.Fprintf(out, "%-9s %d cycles  %.1f%% capacity health\n", "", power.Cycles, power.CapacityHealth)
		}
	}

	if thermal := snapshot.Thermal; thermal != nil {
		fmt.Fprintf(out, "%-9s pressure %d  cpu limit %d%%  scheduler %d%%", "thermal", thermal.Pressure, thermal.CPUSpeedLimit, thermal.SchedulerLimit)
		if len(thermal.FanRPM) > 0 {
			fmt.Fprintf(out, "  fans %v rpm", thermal.FanRPM)
		}
		fmt.Fprintln(out)
	}

	for _, device := range snapshot.Devices {
		fmt.Fprintf(out, "%-9s %-9s %-12s %s\n", "storage", device.Device, device.Status, device.Media)
	}
	for _, device := range snapshot.Bluetooth {
		fmt.Fprintf(out, "%-9s %3d%%  %s\n", "bluetooth", device.Percent, device.Name)
	}
	for _, proxy := range []struct{ name, endpoint string }{{"http", snapshot.Proxy.HTTP}, {"https", snapshot.Proxy.HTTPS}, {"socks", snapshot.Proxy.SOCKS}} {
		if proxy.endpoint != "" {
			fmt.Fprintf(out, "%-9s %-5s %s\n", "proxy", proxy.name, proxy.endpoint)
		}
	}

	if len(snapshot.Processes) > 0 {
		fmt.Fprintf(out, "\n%-9s %-24s %8s %8s\n", "top", "process", "cpu", "memory")
		for _, process := range snapshot.Processes {
			fmt.Fprintf(out, "%-9s %-24s %7.1f%% %7.1f%%\n", "",
				text.Truncate(process.Name, 24), process.CPU, process.Memory)
		}
	}
	for _, alert := range snapshot.Alerts {
		fmt.Fprintf(out, "\n%-9s %s\n", "alert", alert.Message)
	}

	if explain {
		fmt.Fprintf(out, "\n%-9s %-18s %6s %8s %8s\n", "score", "component", "weight", "percent", "penalty")
		for _, component := range metrics.Explain(snapshot) {
			fmt.Fprintf(out, "%-9s %-18s %6.2f %7.1f%% %8.1f\n", "",
				component.Name, component.Weight, component.Percent, component.Penalty)
		}
		fmt.Fprintf(out, "%-9s %-18s %6s %8s %8d\n", "", "load score", "", "", snapshot.LoadScore)
	}

	for _, issue := range snapshot.Issues {
		fmt.Fprintf(out, "\nnote: %s\n", issue)
	}
}

// meter is a fixed-width bar. It is drawn from the same block characters at
// every call so columns line up whatever the value is.
func meter(percent float64) string {
	const width = 18
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := int(percent/100*float64(width) + 0.5)
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func uptime(duration time.Duration) string {
	if duration <= 0 {
		return "unknown"
	}
	days := int(duration.Hours()) / 24
	hours := int(duration.Hours()) % 24
	minutes := int(duration.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}
