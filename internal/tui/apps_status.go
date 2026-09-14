package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dappermint/ratatouille/internal/metrics"
	"github.com/dappermint/ratatouille/internal/safety"
	"github.com/dappermint/ratatouille/internal/storage"
	"github.com/dappermint/ratatouille/internal/text"
	"github.com/dappermint/ratatouille/internal/uninstall"
)

func (state *tuiState) appSummary(width int) string {
	var selected int
	var bytes int64
	for _, app := range state.apps {
		if state.selectedApps[app.Path] {
			selected++
			bytes += app.Bytes
		}
	}
	line := fmt.Sprintf("apps=%d  selected=%d  reclaim=%s", len(state.apps), selected, storage.HumanBytes(bytes))
	colour := colorFog
	if selected > 0 {
		colour = colorAmber
	}
	return state.paint(colour, text.Truncate(line, width))
}

func (state *tuiState) appTableHeader(width int) string {
	return state.paint(colorFog, text.Truncate(state.t("tui.apps.header"), width))
}

func (state *tuiState) appLine(app uninstall.App, focused bool, width int) string {
	cursor := " "
	if focused {
		cursor = state.paint(colorCyan, "┃")
	}
	mark := "[-]"
	if !app.Protected {
		mark = "[ ]"
		if state.selectedApps[app.Path] {
			mark = state.paint(colorAmber, "[x]")
		}
	}
	name := app.Name
	if app.Version != "" {
		name += " " + app.Version
	}
	plain := fmt.Sprintf("%s %s %9s  %-7s %s", cursor, mark, storage.HumanBytes(app.Bytes), app.Scope, text.Clean(name))
	return text.Truncate(plain, width)
}

func (state *tuiState) appInspector(width, lineCount int) []string {
	if state.cursor() < 0 || state.cursor() >= len(state.apps) {
		return padLines([]string{state.paint(colorFog, text.Truncate(state.t("tui.apps.none"), width))}, lineCount)
	}
	app := state.apps[state.cursor()]
	meta := storage.HumanBytes(app.Bytes) + " / " + string(app.Scope)
	lines := []string{state.bold(colorInk, text.Truncate(text.JoinEdges(app.Name, meta, width), width))}
	if lineCount > 1 {
		lines = append(lines, state.paint(colorFog, text.Truncate("bundle  "+app.Bundle, width)))
	}
	if lineCount > 2 {
		lines = append(lines, state.paint(colorFog, text.Truncate("path    "+app.Path, width)))
	}
	if lineCount > 3 {
		action := "space marks, c uninstalls through Trash"
		if app.Protected {
			action = "protected: " + app.Reason
		}
		lines = append(lines, state.paint(colorAmber, text.Truncate(action, width)))
	}
	return padLines(lines, lineCount)
}

func (state *tuiState) toggleApp() {
	if state.cursor() < 0 || state.cursor() >= len(state.apps) {
		return
	}
	app := state.apps[state.cursor()]
	if app.Protected {
		state.notice = "protected: " + app.Reason
		return
	}
	state.selectedApps[app.Path] = !state.selectedApps[app.Path]
}

func (state *tuiState) executeApps(ctx context.Context, home string, rootful bool, identity *storage.CommandIdentity, funnel *safety.Funnel, terminal *rawTerminal, renderer *screenRenderer, out io.Writer) error {
	var selected []uninstall.App
	for _, app := range state.apps {
		if state.selectedApps[app.Path] && !app.Protected {
			selected = append(selected, app)
		}
	}
	if len(selected) == 0 {
		state.notice = "apps selected=0"
		return nil
	}
	terminal.Restore()
	renderer.Exit()
	fmt.Fprintln(out, "\nthis will uninstall:")
	for _, app := range selected {
		fmt.Fprintf(out, "  %-32s %10s  %s\n", app.Name, storage.HumanBytes(app.Bytes), app.Path)
	}
	fmt.Fprint(out, "\ntype \"uninstall\" to continue: ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(answer) == "uninstall" {
		env := uninstall.Env{Home: home, Rootful: rootful, Identity: identity}
		for _, app := range selected {
			fmt.Fprintf(out, "\n%s\n", app.Name)
			if cask := uninstall.BrewCask(ctx, app, identity); cask != "" {
				if err := uninstall.BrewUninstall(ctx, funnel, cask, out); err != nil {
					fmt.Fprintf(out, "  failed: %v\n", err)
					continue
				}
				result := uninstall.Run(ctx, funnel, env, app, state.apps, uninstall.Options{LeftoversOnly: true, Rootful: rootful}, out)
				for _, failure := range result.Errors {
					fmt.Fprintf(out, "  failed: %s\n", failure)
				}
				continue
			}
			result := uninstall.Run(ctx, funnel, env, app, state.apps, uninstall.Options{Rootful: rootful}, out)
			for _, failure := range result.Errors {
				fmt.Fprintf(out, "  failed: %s\n", failure)
			}
		}
	} else {
		fmt.Fprintln(out, "aborted")
	}
	fmt.Fprint(out, "\nenter: return ")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	renderer.Enter()
	if err := terminal.Enter(); err != nil {
		return err
	}
	state.apps = uninstall.Inventory(ctx, uninstall.Env{Home: home, Rootful: rootful, Identity: identity})
	state.selectedApps = make(map[string]bool)
	state.setCursor(0)
	state.setOffset(0)
	return nil
}

type statusRow struct {
	name, value, detail, colour string
}

func (state *tuiState) liveStatusSummary(width int) string {
	snapshot := state.status
	line := fmt.Sprintf("load=%d/%s  cpu=%.1f%%  memory=%.1f%%  sampled=%s",
		snapshot.LoadScore, metrics.Level(snapshot.LoadScore), snapshot.CPU.Busy, snapshot.Memory.Pressure, text.Duration(snapshot.Elapsed))
	return state.paint(scoreColour(snapshot.LoadScore), text.Truncate(line, width))
}

func (state *tuiState) statusRows() []statusRow {
	snapshot := state.status
	rows := []statusRow{
		{name: "cpu", value: fmt.Sprintf("%.1f%%", snapshot.CPU.Busy), detail: fmt.Sprintf("user %.1f%%, system %.1f%%, load %.2f %.2f %.2f", snapshot.CPU.User, snapshot.CPU.System, snapshot.CPU.Load[0], snapshot.CPU.Load[1], snapshot.CPU.Load[2]), colour: pressureColour(int(snapshot.CPU.Busy))},
		{name: "memory", value: fmt.Sprintf("%.1f%%", snapshot.Memory.Pressure), detail: storage.HumanBytes(snapshot.Memory.Used) + " used, " + storage.HumanBytes(snapshot.Memory.Available) + " available", colour: pressureColour(int(snapshot.Memory.Pressure))},
	}
	if snapshot.Memory.SwapTotal > 0 {
		rows = append(rows, statusRow{name: "swap", value: storage.HumanBytes(snapshot.Memory.SwapUsed), detail: storage.HumanBytes(snapshot.Memory.SwapTotal) + " total", colour: colorAmber})
	}
	if snapshot.GPU != nil {
		rows = append(rows, statusRow{name: "gpu", value: fmt.Sprintf("%.1f%%", snapshot.GPU.Device), detail: fmt.Sprintf("renderer %.1f%%, tiler %.1f%%", snapshot.GPU.Renderer, snapshot.GPU.Tiler), colour: pressureColour(int(snapshot.GPU.Device))})
	}
	for _, disk := range snapshot.Disks {
		rows = append(rows, statusRow{name: "disk " + disk.Device, value: fmt.Sprintf("%.1f MB/s", disk.Throughput), detail: fmt.Sprintf("%.1f transfers/s, %.1f KB/transfer", disk.Transfers, disk.KBPerAccess), colour: colorCyan})
	}
	if snapshot.Network.Interface != "" {
		rows = append(rows, statusRow{name: "network", value: snapshot.Network.Interface, detail: "down " + storage.HumanBytes(int64(snapshot.Network.DownRate)) + "/s, up " + storage.HumanBytes(int64(snapshot.Network.UpRate)) + "/s", colour: colorCyan})
	}
	if snapshot.Power != nil {
		detail := snapshot.Power.State + " " + snapshot.Power.Remaining
		if snapshot.Power.Cycles > 0 {
			detail += fmt.Sprintf(", %d cycles, %.1f%% capacity health", snapshot.Power.Cycles, snapshot.Power.CapacityHealth)
		}
		rows = append(rows, statusRow{name: "power", value: fmt.Sprintf("%d%%", snapshot.Power.Percent), detail: detail, colour: colorMint})
	}
	if snapshot.Thermal != nil {
		rows = append(rows, statusRow{name: "thermal", value: fmt.Sprintf("pressure %d", snapshot.Thermal.Pressure), detail: fmt.Sprintf("cpu limit %d%%, scheduler %d%%, fans %v rpm", snapshot.Thermal.CPUSpeedLimit, snapshot.Thermal.SchedulerLimit, snapshot.Thermal.FanRPM), colour: colorAmber})
	}
	for _, device := range snapshot.Devices {
		rows = append(rows, statusRow{name: "storage " + device.Device, value: device.Status, detail: device.Media, colour: colorCyan})
	}
	for _, device := range snapshot.Bluetooth {
		rows = append(rows, statusRow{name: "bluetooth", value: fmt.Sprintf("%d%%", device.Percent), detail: device.Name, colour: colorCyan})
	}
	proxies := []struct{ name, endpoint string }{
		{"http proxy", snapshot.Proxy.HTTP},
		{"https proxy", snapshot.Proxy.HTTPS},
		{"socks proxy", snapshot.Proxy.SOCKS},
	}
	for _, proxy := range proxies {
		if proxy.endpoint != "" {
			rows = append(rows, statusRow{name: proxy.name, value: "enabled", detail: proxy.endpoint, colour: colorAmber})
		}
	}
	for _, process := range snapshot.Processes {
		rows = append(rows, statusRow{name: "process", value: fmt.Sprintf("%.1f%% cpu", process.CPU), detail: fmt.Sprintf("%s, %.1f%% memory", process.Name, process.Memory), colour: colorFog})
	}
	for _, alert := range snapshot.Alerts {
		rows = append(rows, statusRow{name: "alert", value: alert.Kind, detail: alert.Message, colour: colorCoral})
	}
	return rows
}

func (state *tuiState) statusMetricLine(row statusRow, focused bool, width int) string {
	cursor := " "
	if focused {
		cursor = state.paint(colorCyan, "┃")
	}
	line := fmt.Sprintf("%s %-14s %12s  %s", cursor, row.name, row.value, row.detail)
	return state.paint(row.colour, text.Truncate(line, width))
}

func (state *tuiState) statusInspector(width, lineCount int) []string {
	rows := state.statusRows()
	if state.cursor() < 0 || state.cursor() >= len(rows) {
		return padLines([]string{state.paint(colorFog, text.Truncate(state.t("tui.status.none"), width))}, lineCount)
	}
	row := rows[state.cursor()]
	lines := []string{state.bold(row.colour, text.Truncate(text.JoinEdges(row.name, row.value, width), width))}
	for _, line := range text.Wrap(row.detail, width) {
		lines = append(lines, state.paint(colorFog, text.Truncate(line, width)))
	}
	return padLines(lines, lineCount)
}

func pressureColour(percent int) string {
	if percent >= 75 {
		return colorCoral
	}
	if percent >= 45 {
		return colorAmber
	}
	return colorMint
}

func scoreColour(score int) string {
	if score >= 75 {
		return colorMint
	}
	if score >= 45 {
		return colorAmber
	}
	return colorCoral
}
