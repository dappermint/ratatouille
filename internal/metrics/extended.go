package metrics

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dappermint/ratatouille/internal/plist"
	"github.com/dappermint/ratatouille/internal/storage"
)

var ioregMetric = regexp.MustCompile(`"([^"]+)"\s*=\s*([0-9]+)`) // ioreg's text dictionary form

func enrichPower(ctx context.Context, power *Power) {
	output, err := storage.CaptureCommand(ctx, quickTimeout, "/usr/sbin/ioreg", "-r", "-c", "AppleSmartBattery", "-a")
	if err != nil {
		return
	}
	value, err := plist.ParseAny([]byte(output))
	if err != nil {
		return
	}
	values, ok := value.([]any)
	if !ok || len(values) == 0 {
		return
	}
	dict, ok := values[0].(plist.Dict)
	if !ok {
		return
	}
	if cycles, ok := dict.Int("CycleCount"); ok {
		power.Cycles = int(cycles)
	}
	if capacity, ok := dict.Int("AppleRawMaxCapacity"); ok {
		power.MaxCapacity = capacity
	} else if capacity, ok := dict.Int("MaxCapacity"); ok {
		power.MaxCapacity = capacity
	}
	if capacity, ok := dict.Int("DesignCapacity"); ok {
		power.DesignCapacity = capacity
	}
	if power.MaxCapacity > 0 && power.DesignCapacity > 0 {
		power.CapacityHealth = float64(power.MaxCapacity) / float64(power.DesignCapacity) * 100
	}
}

func readGPU(ctx context.Context) *GPU {
	output, err := storage.CaptureCommand(ctx, quickTimeout, "/usr/sbin/ioreg", "-r", "-c", "IOAccelerator", "-l")
	if err != nil {
		return nil
	}
	gpu := &GPU{}
	found := false
	for _, match := range ioregMetric.FindAllStringSubmatch(output, -1) {
		value := parseFloat(match[2])
		switch match[1] {
		case "Device Utilization %":
			gpu.Device, found = value, true
		case "Renderer Utilization %":
			gpu.Renderer, found = value, true
		case "Tiler Utilization %":
			gpu.Tiler, found = value, true
		}
	}
	if !found {
		return nil
	}
	return gpu
}

func readThermal(ctx context.Context) *Thermal {
	thermal := &Thermal{}
	known := false
	if output, err := storage.CaptureCommand(ctx, quickTimeout, "/usr/bin/pmset", "-g", "therm"); err == nil {
		for line := range strings.SplitSeq(output, "\n") {
			key, value, found := strings.Cut(line, "=")
			if !found {
				continue
			}
			switch strings.TrimSpace(key) {
			case "CPU_Speed_Limit":
				thermal.CPUSpeedLimit, known = atoi(value), true
			case "Scheduler_Limit":
				thermal.SchedulerLimit, known = atoi(value), true
			}
		}
	}
	if output, err := storage.CaptureCommand(ctx, quickTimeout, "/usr/sbin/sysctl", "-n", "kern.thermal_pressure"); err == nil {
		thermal.Pressure, known = atoi(output), true
	}
	if output, err := storage.CaptureCommand(ctx, quickTimeout, "/usr/sbin/ioreg", "-r", "-c", "AppleSMC", "-l"); err == nil {
		for _, match := range ioregMetric.FindAllStringSubmatch(output, -1) {
			if strings.Contains(strings.ToLower(match[1]), "fan") && strings.Contains(strings.ToLower(match[1]), "speed") {
				thermal.FanRPM = append(thermal.FanRPM, atoi(match[2]))
				known = true
			}
		}
	}
	if !known {
		return nil
	}
	return thermal
}

func readProxy(ctx context.Context) Proxy {
	output, err := storage.CaptureCommand(ctx, quickTimeout, "/usr/sbin/scutil", "--proxy")
	if err != nil {
		return Proxy{}
	}
	values := make(map[string]string)
	for line := range strings.SplitSeq(output, "\n") {
		key, value, found := strings.Cut(line, ":")
		if found {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	endpoint := func(prefix string) string {
		if values[prefix+"Enable"] != "1" || values[prefix+"Proxy"] == "" {
			return ""
		}
		return values[prefix+"Proxy"] + ":" + values[prefix+"Port"]
	}
	return Proxy{HTTP: endpoint("HTTP"), HTTPS: endpoint("HTTPS"), SOCKS: endpoint("SOCKS")}
}

func readBluetooth(ctx context.Context) []BluetoothDevice {
	output, err := storage.CaptureCommand(ctx, quickTimeout, "/usr/sbin/ioreg", "-r", "-c", "AppleDeviceManagementHIDEventService", "-a")
	if err != nil {
		return nil
	}
	value, err := plist.ParseAny([]byte(output))
	if err != nil {
		return nil
	}
	values, _ := value.([]any)
	var devices []BluetoothDevice
	for _, value := range values {
		dict, ok := value.(plist.Dict)
		if !ok {
			continue
		}
		name, _ := dict.String("Product")
		percent, ok := dict.Int("BatteryPercent")
		if !ok || name == "" || percent < 0 || percent > 100 {
			continue
		}
		devices = append(devices, BluetoothDevice{Name: name, Percent: int(percent)})
	}
	return devices
}

var storageDeviceCache struct {
	sync.Mutex
	at      time.Time
	devices []storage.StorageDevice
	issue   string
}

func readStorageDevices(ctx context.Context) ([]storage.StorageDevice, string) {
	storageDeviceCache.Lock()
	defer storageDeviceCache.Unlock()
	if time.Since(storageDeviceCache.at) < 30*time.Second {
		return append([]storage.StorageDevice(nil), storageDeviceCache.devices...), storageDeviceCache.issue
	}
	devices, issue := collectStorageDevices(ctx)
	storageDeviceCache.at = time.Now()
	storageDeviceCache.devices = append([]storage.StorageDevice(nil), devices...)
	storageDeviceCache.issue = issue
	return devices, issue
}

func collectStorageDevices(ctx context.Context) ([]storage.StorageDevice, string) {
	mounts, err := storage.MountedFilesystems()
	if err != nil {
		return nil, "storage devices: " + storage.CompactError(err)
	}
	containers, err := storage.APFSContainers(ctx, quickTimeout, mounts)
	if err != nil {
		return nil, "storage devices: " + storage.CompactError(err)
	}
	devices, issues := storage.StorageDevices(ctx, quickTimeout, containers)
	return devices, strings.Join(issues, "; ")
}

type Tracker struct {
	history   []HistoryPoint
	hotCPU    map[int]int
	hotMemory map[int]int
}

func NewTracker() *Tracker {
	return &Tracker{hotCPU: make(map[int]int), hotMemory: make(map[int]int)}
}

func (tracker *Tracker) Observe(snapshot Snapshot) Snapshot {
	point := HistoryPoint{At: snapshot.At, CPU: snapshot.CPU.Busy, Memory: snapshot.Memory.Pressure, NetworkDown: snapshot.Network.DownRate, NetworkUp: snapshot.Network.UpRate}
	tracker.history = append(tracker.history, point)
	if len(tracker.history) > 60 {
		tracker.history = tracker.history[len(tracker.history)-60:]
	}
	snapshot.History = append([]HistoryPoint(nil), tracker.history...)
	seen := make(map[int]bool)
	for _, process := range snapshot.Processes {
		seen[process.PID] = true
		tracker.hotCPU[process.PID] = nextCount(tracker.hotCPU[process.PID], process.CPU >= 80)
		tracker.hotMemory[process.PID] = nextCount(tracker.hotMemory[process.PID], process.Memory >= 25)
		if tracker.hotCPU[process.PID] >= 3 {
			snapshot.Alerts = append(snapshot.Alerts, Alert{Kind: "sustained-cpu", Message: fmt.Sprintf("%s stayed above 80%% CPU", process.Name), PID: process.PID, Samples: tracker.hotCPU[process.PID]})
		}
		if tracker.hotMemory[process.PID] >= 3 {
			snapshot.Alerts = append(snapshot.Alerts, Alert{Kind: "sustained-memory", Message: fmt.Sprintf("%s stayed above 25%% memory", process.Name), PID: process.PID, Samples: tracker.hotMemory[process.PID]})
		}
	}
	for pid := range tracker.hotCPU {
		if !seen[pid] {
			delete(tracker.hotCPU, pid)
			delete(tracker.hotMemory, pid)
		}
	}
	return snapshot
}

func nextCount(previous int, hot bool) int {
	if hot {
		return previous + 1
	}
	return 0
}
