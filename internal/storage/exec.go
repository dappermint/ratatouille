package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"
)

func CompactError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return text
}

func UniqueStrings(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// tailBytes is kept from the end of an oversized stream. Command summaries
// ("N store paths would be deleted") are the last line, so head-only
// truncation would drop exactly the part every caller parses.
const tailBytes = 8 << 10

type cappedBuffer struct {
	buffer   bytes.Buffer
	tail     []byte
	limit    int
	overflow bool
}

type CommandIdentity struct {
	UID      uint32
	GID      uint32
	Groups   []uint32
	Username string
	Home     string
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		head := data
		if len(head) > remaining {
			head = head[:remaining]
		}
		_, _ = b.buffer.Write(head)
		data = data[len(head):]
	}
	if len(data) > 0 {
		b.overflow = true
		b.tail = append(b.tail, data...)
		if len(b.tail) > tailBytes {
			b.tail = append(b.tail[:0], b.tail[len(b.tail)-tailBytes:]...)
		}
	}
	return original, nil
}

func (b *cappedBuffer) String() string {
	if !b.overflow {
		return b.buffer.String()
	}
	return b.buffer.String() + "\n…truncated…\n" + string(b.tail)
}

func CaptureCommand(ctx context.Context, timeout time.Duration, command string, args ...string) (string, error) {
	return CaptureCommandAs(ctx, timeout, nil, command, args...)
}

func CaptureCommandAs(ctx context.Context, timeout time.Duration, identity *CommandIdentity, command string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	output := &cappedBuffer{limit: 2 * 1024 * 1024}
	cmd := exec.CommandContext(commandCtx, command, args...)
	ApplyCommandIdentity(cmd, identity)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return output.String(), fmt.Errorf("timed out after %s", timeout)
	}
	return output.String(), err
}

func ApplyCommandIdentity(cmd *exec.Cmd, identity *CommandIdentity) {
	if identity == nil {
		return
	}
	cmd.Dir = identity.Home
	cmd.Env = CommandEnvironment(os.Environ(), identity)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid:    identity.UID,
		Gid:    identity.GID,
		Groups: identity.Groups,
	}}
}

func CommandEnvironment(environment []string, identity *CommandIdentity) []string {
	filtered := make([]string, 0, len(environment)+3)
	for _, entry := range environment {
		if strings.HasPrefix(entry, "HOME=") || strings.HasPrefix(entry, "USER=") || strings.HasPrefix(entry, "LOGNAME=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered,
		"HOME="+identity.Home,
		"USER="+identity.Username,
		"LOGNAME="+identity.Username,
	)
}
