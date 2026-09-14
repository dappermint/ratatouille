package metrics

import (
	"math"
	"testing"
	"time"
)

// Real output captured from a running machine. The parsers are the part that
// breaks when a macOS release moves a column, so they are tested against the
// exact shape they have to survive.
const iostatOutput = `              disk0               disk4               disk5       cpu    load average
    KB/t  tps  MB/s     KB/t  tps  MB/s     KB/t  tps  MB/s  us sy id   1m   5m   15m
   19.35  521  9.84     6.53    0  0.00     4.03    0  0.00  13 12 74  18.79 10.24 10.68
   20.26 1101 21.79     0.00    0  0.00     0.00    0  0.00  76 24  0  18.79 10.24 10.68
`

const vmStatOutput = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                     8326.
Pages active:                                 266842.
Pages inactive:                               246942.
Pages speculative:                             19335.
Pages throttled:                                   0.
Pages wired down:                             191769.
Pages purgeable:                                6645.
"Translation faults":                     2256289614.
Anonymous pages:                              334661.
Pages stored in compressor:                   514539.
Pages occupied by compressor:                 274005.
`

const netstatOutput = `Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                        434545     0   63877439   434545     0   63877439     0
lo0        16384 127           127.0.0.1         434545     -   63877439   434545     -   63877439     -
en0        1500  <Link#12>   aa:bb:cc:dd:ee:ff  1000000     0 5000000000   900000     0  400000000     0
awdl0      1500  <Link#14>   11:22:33:44:55:66      100     0       1000       50     0        500     0
`

const psOutput = `  PID  %CPU %MEM COMM
 2860  75.9  0.1 ecosystemd
  460  45.9  0.5 WindowServer
  491  28.7  0.1 trustd
`

// The header names the disks and then the words "cpu" and "load average".
// Treating those words as devices shifts every column and silently reads CPU as
// zero, which is exactly what happened.
func TestParseIOStatTakesTheSecondSample(t *testing.T) {
	cpu, disks, issue := parseIOStat(iostatOutput)
	if issue != "" {
		t.Fatalf("issue: %s", issue)
	}
	if len(disks) != 3 {
		t.Fatalf("got %d disks, want 3: %+v", len(disks), disks)
	}
	if disks[0].Device != "disk0" || disks[2].Device != "disk5" {
		t.Errorf("device names wrong: %+v", disks)
	}
	// The second sample, not the first: the first row is an average since boot.
	if cpu.User != 76 || cpu.System != 24 || cpu.Idle != 0 {
		t.Errorf("cpu = %+v, want the second sample 76/24/0", cpu)
	}
	if cpu.Busy != 100 {
		t.Errorf("busy = %v, want 100", cpu.Busy)
	}
	if cpu.Load != [3]float64{18.79, 10.24, 10.68} {
		t.Errorf("load = %v", cpu.Load)
	}
	if disks[0].Throughput != 21.79 || disks[0].Transfers != 1101 {
		t.Errorf("disk0 = %+v, want the second sample", disks[0])
	}
}

func TestParseIOStatRefusesShortOutput(t *testing.T) {
	for _, output := range []string{"", "one line\n", "header\ncolumns\nonly one row\n"} {
		if _, _, issue := parseIOStat(output); issue == "" {
			t.Errorf("accepted output it cannot measure a delta from: %q", output)
		}
	}
}

func TestDeviceNamesStopAtTheLabels(t *testing.T) {
	got := deviceNames("              disk0               disk4       cpu    load average")
	if len(got) != 2 || got[0] != "disk0" || got[1] != "disk4" {
		t.Errorf("deviceNames = %v, want the two disks only", got)
	}
}

func TestParseVMStat(t *testing.T) {
	pages, pageSize := parseVMStat(vmStatOutput)
	if pageSize != 16384 {
		t.Fatalf("page size = %d, want 16384", pageSize)
	}
	cases := map[string]int64{
		"free":                   8326,
		"active":                 266842,
		"inactive":               246942,
		"speculative":            19335,
		"wired down":             191769,
		"purgeable":              6645,
		"anonymous":              334661,
		"occupied by compressor": 274005,
	}
	for label, want := range cases {
		if got := pages[label]; got != want {
			t.Errorf("pages[%q] = %d, want %d", label, got, want)
		}
	}
}

// Used and Available are not complements. A test that asserted they sum to the
// total would be asserting a falsehood about macOS.
func TestMemoryReportsBothViewsWithoutClaimingTheyComplement(t *testing.T) {
	pages, pageSize := parseVMStat(vmStatOutput)
	total := int64(17179869184)

	memory := Memory{Total: total}
	memory.Free = pages["free"] * pageSize
	memory.Wired = pages["wired down"] * pageSize
	memory.Compressed = pages["occupied by compressor"] * pageSize
	memory.App = max(pages["anonymous"]-pages["purgeable"], 0) * pageSize
	memory.Used = memory.App + memory.Wired + memory.Compressed
	memory.Available = (pages["free"] + pages["inactive"] + pages["speculative"] + pages["purgeable"]) * pageSize
	memory.UsedShare = float64(memory.Used) / float64(total) * 100
	memory.AvailableShare = float64(memory.Available) / float64(total) * 100
	memory.Pressure = 100 - memory.AvailableShare

	if memory.Used <= 0 || memory.Used > total {
		t.Errorf("used = %d, outside 0..%d", memory.Used, total)
	}
	if memory.Available <= 0 || memory.Available > total {
		t.Errorf("available = %d, outside 0..%d", memory.Available, total)
	}
	if math.Abs(memory.Pressure+memory.AvailableShare-100) > 0.001 {
		t.Errorf("pressure and available do not complement: %.2f + %.2f", memory.Pressure, memory.AvailableShare)
	}
	if memory.App <= 0 {
		t.Error("app memory was not derived")
	}
}

func TestParseSwap(t *testing.T) {
	total, used := parseSwap("total = 2048.00M  used = 1396.75M  free = 651.25M  (encrypted)")
	if total != 2048*1024*1024 {
		t.Errorf("total = %d", total)
	}
	if used != int64(1396.75*1024*1024) {
		t.Errorf("used = %d", used)
	}
}

// Loopback traffic is a machine talking to itself and is not network use.
func TestParseNetstatSkipsLoopbackAndDuplicates(t *testing.T) {
	down, up, busiest := parseNetstat(netstatOutput)
	if busiest != "en0" {
		t.Errorf("busiest = %q, want en0", busiest)
	}
	if down != 5000000000+1000 {
		t.Errorf("down = %d, want en0 plus awdl0 without loopback", down)
	}
	if up != 400000000+500 {
		t.Errorf("up = %d", up)
	}
}

func TestParseProcesses(t *testing.T) {
	processes := parseProcesses(psOutput, 2)
	if len(processes) != 2 {
		t.Fatalf("got %d processes, want the limit of 2", len(processes))
	}
	if processes[0].Name != "ecosystemd" || processes[0].PID != 2860 || processes[0].CPU != 75.9 {
		t.Errorf("first process = %+v", processes[0])
	}
}

func TestParsePower(t *testing.T) {
	power := parsePower("Now drawing from 'AC Power'\n -InternalBattery-0 (id=23789667)\t80%; AC attached; not charging present: true\n")
	if power == nil {
		t.Fatal("no power reading")
	}
	if power.Percent != 80 || power.Source != "AC Power" {
		t.Errorf("power = %+v", power)
	}

	discharging := parsePower("Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t55%; discharging; 3:21 remaining present: true\n")
	if discharging == nil || discharging.Percent != 55 || discharging.State != "discharging" {
		t.Errorf("power = %+v", discharging)
	}
}

// A desktop has no battery, which is not an error and must not be reported as
// a reading of zero percent.
func TestParsePowerOnAMachineWithoutABattery(t *testing.T) {
	if power := parsePower("Now drawing from 'AC Power'\n"); power != nil {
		t.Errorf("invented a battery: %+v", power)
	}
}

func TestUptimeFrom(t *testing.T) {
	line := "{ sec = 1785486206, usec = 176574 } Fri Jul 31 12:23:26 2026"
	if got := uptimeFrom(line); got <= 0 {
		t.Errorf("uptime = %s, want a positive duration", got)
	}
	for _, bad := range []string{"", "nonsense", "{ usec = 1 }", "{ sec = notanumber }"} {
		if got := uptimeFrom(bad); got != 0 {
			t.Errorf("uptimeFrom(%q) = %s, want zero", bad, got)
		}
	}
	// A boot time in the future is a clock problem, not a negative uptime.
	future := time.Now().Add(time.Hour).Unix()
	if got := uptimeFrom("{ sec = " + itoa(future) + ", usec = 0 }"); got != 0 {
		t.Errorf("a future boot time produced %s", got)
	}
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestScoreIsBoundedAndExplained(t *testing.T) {
	idle := Snapshot{}
	if got := Score(idle); got != 100 {
		t.Errorf("an idle machine scored %d, want 100", got)
	}

	saturated := Snapshot{
		CPU:    CPU{Busy: 100},
		Memory: Memory{Total: 100, Wired: 100, Pressure: 100, SwapTotal: 100, SwapUsed: 100},
	}
	if got := Score(saturated); got != 0 {
		t.Errorf("a saturated machine scored %d, want 0", got)
	}

	// The weights have to add up, or the score cannot reach either end.
	var weight float64
	for _, component := range Explain(saturated) {
		weight += component.Weight
	}
	if math.Abs(weight-1) > 0.0001 {
		t.Errorf("weights sum to %v, want 1", weight)
	}
}

func TestLevelBands(t *testing.T) {
	cases := map[int]string{100: "idle", 80: "idle", 60: "working", 30: "busy", 10: "saturated", 0: "saturated"}
	for score, want := range cases {
		if got := Level(score); got != want {
			t.Errorf("Level(%d) = %q, want %q", score, got, want)
		}
	}
}

func TestIORegMetricsAllowAppleSpacing(t *testing.T) {
	matches := ioregMetric.FindAllStringSubmatch(`"Device Utilization %" = 48,"Renderer Utilization %"=31`, -1)
	if len(matches) != 2 || matches[0][2] != "48" || matches[1][2] != "31" {
		t.Fatalf("matches = %v", matches)
	}
}

func TestTrackerOnlyAlertsAfterThreeHotSamples(t *testing.T) {
	tracker := NewTracker()
	snapshot := Snapshot{At: time.Now(), Processes: []Process{{PID: 42, Name: "worker", CPU: 95}}}
	for sample := 1; sample <= 3; sample++ {
		observed := tracker.Observe(snapshot)
		if sample < 3 && len(observed.Alerts) != 0 {
			t.Fatalf("alerted after %d samples: %+v", sample, observed.Alerts)
		}
		if sample == 3 && (len(observed.Alerts) != 1 || observed.Alerts[0].Kind != "sustained-cpu") {
			t.Fatalf("third sample did not alert: %+v", observed.Alerts)
		}
	}
}
