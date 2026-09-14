package storage

import (
	"strings"
	"testing"
)

// `nix store gc --dry-run` prints every dead path (megabytes of it) before the
// one line callers parse. Head-only truncation silently dropped that summary.
func TestCappedBufferKeepsTail(t *testing.T) {
	buffer := &cappedBuffer{limit: 1024}
	_, _ = buffer.Write([]byte(strings.Repeat("/nix/store/dead\n", 4096)))
	_, _ = buffer.Write([]byte("32973 store paths would be deleted\n"))

	output := buffer.String()
	if !strings.Contains(output, "32973 store paths would be deleted") {
		t.Fatalf("summary line lost, got %d bytes ending %q", len(output), output[max(0, len(output)-60):])
	}
	if !strings.HasPrefix(output, "/nix/store/dead\n") {
		t.Fatalf("head lost: %q", output[:min(40, len(output))])
	}
	if len(output) > 1024+tailBytes+32 {
		t.Fatalf("buffer unbounded: %d bytes", len(output))
	}
}
