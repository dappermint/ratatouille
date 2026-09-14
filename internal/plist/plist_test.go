package plist

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// assertSample checks the same expectations against both encodings, which is
// the point: a caller must not be able to tell which one it got.
func assertSample(t *testing.T, dict Dict) {
	t.Helper()

	if got, _ := dict.String("CFBundleIdentifier"); got != "com.example.app" {
		t.Errorf("CFBundleIdentifier = %q", got)
	}
	if got, _ := dict.String("CFBundleName"); got != "Example ünïcode" {
		t.Errorf("CFBundleName = %q, want the non-ascii name intact", got)
	}
	if got, _ := dict.String("CFBundleShortVersionString"); got != "2.5.1" {
		t.Errorf("version = %q", got)
	}
	if got, ok := dict.Int("NumericThing"); !ok || got != 42 {
		t.Errorf("NumericThing = %d, ok=%v", got, ok)
	}
	if got, ok := dict.Bool("BoolTrue"); !ok || !got {
		t.Errorf("BoolTrue = %v, ok=%v", got, ok)
	}
	if got, ok := dict.Bool("BoolFalse"); !ok || got {
		t.Errorf("BoolFalse = %v, ok=%v", got, ok)
	}
	arguments := dict.Strings("ProgramArguments")
	if len(arguments) != 2 || arguments[0] != "/usr/local/bin/helper" || arguments[1] != "--serve" {
		t.Errorf("ProgramArguments = %v", arguments)
	}
	nested, ok := dict.Dict("Nested")
	if !ok {
		t.Fatal("Nested is not a dictionary")
	}
	if got, _ := nested.String("Inner"); got != "value" {
		t.Errorf("Nested.Inner = %q", got)
	}
}

func TestParseXML(t *testing.T) {
	dict, err := ReadFile(filepath.Join("testdata", "sample.xml"))
	if err != nil {
		t.Fatalf("reading the xml fixture: %v", err)
	}
	assertSample(t, dict)
}

func TestParseBinary(t *testing.T) {
	dict, err := ReadFile(filepath.Join("testdata", "sample.binary.plist"))
	if err != nil {
		t.Fatalf("reading the binary fixture: %v", err)
	}
	assertSample(t, dict)
}

func TestBothEncodingsAgree(t *testing.T) {
	fromXML, err := ReadFile(filepath.Join("testdata", "sample.xml"))
	if err != nil {
		t.Fatalf("xml: %v", err)
	}
	fromBinary, err := ReadFile(filepath.Join("testdata", "sample.binary.plist"))
	if err != nil {
		t.Fatalf("binary: %v", err)
	}
	for key, want := range fromXML {
		got, present := fromBinary[key]
		if !present {
			t.Errorf("the binary form is missing %q", key)
			continue
		}
		if _, isContainer := want.(Dict); isContainer {
			continue
		}
		if _, isArray := want.([]any); isArray {
			continue
		}
		if got != want {
			t.Errorf("%q: xml=%v binary=%v", key, want, got)
		}
	}
}

// A parser that accepts nonsense is worse than one that fails, because the
// caller then acts on it. Every one of these must be an error, not a value.
func TestMalformedInputIsAnErrorNotAValue(t *testing.T) {
	good, err := os.ReadFile(filepath.Join("testdata", "sample.binary.plist"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	cases := map[string][]byte{
		"empty":              {},
		"plain text":         []byte("File Doesn't Exist, Will Create: /tmp/x\n"),
		"html":               []byte("<html><body>not a plist</body></html>"),
		"binary magic only":  []byte("bplist00"),
		"truncated trailer":  good[:len(good)-8],
		"truncated body":     append([]byte{}, append(good[:16], good[len(good)-32:]...)...),
		"xml without plist":  []byte(`<?xml version="1.0"?><other><dict/></other>`),
		"xml unclosed":       []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>a</key>`),
		"binary bad offsets": corrupt(good, len(good)-8, 0xFF),
		"binary bad widths":  corrupt(good, len(good)-26, 0x00),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			value, err := Parse(data)
			if err == nil {
				t.Fatalf("accepted %s and returned %#v", name, value)
			}
		})
	}
}

func corrupt(data []byte, index int, value byte) []byte {
	copied := append([]byte(nil), data...)
	if index >= 0 && index < len(copied) {
		copied[index] = value
	}
	return copied
}

func TestReadFileRefusesSomethingTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.plist")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := file.Truncate(maxFileBytes + 1); err != nil {
		t.Fatalf("sizing: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if _, err := ReadFile(path); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestMissingKeysReportAbsenceRatherThanZero(t *testing.T) {
	dict := Dict{}
	if _, ok := dict.String("nope"); ok {
		t.Error("a missing string reported ok")
	}
	if _, ok := dict.Int("nope"); ok {
		t.Error("a missing integer reported ok")
	}
	if _, ok := dict.Bool("nope"); ok {
		t.Error("a missing bool reported ok")
	}
	if values := dict.Strings("nope"); values != nil {
		t.Errorf("a missing array returned %v", values)
	}
}

// The fixtures are hand-picked. This runs the parser over whatever real Apple
// and third-party bundles the machine actually has, which is the only corpus
// that catches an encoding nobody thought to write a fixture for.
func TestRealBundlesOnThisMachine(t *testing.T) {
	roots := []string{"/Applications", "/System/Applications", "/System/Library/CoreServices"}
	parsed, failures := 0, 0
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || filepath.Ext(entry.Name()) != ".app" {
				continue
			}
			path := filepath.Join(root, entry.Name(), "Contents", "Info.plist")
			dict, err := ReadFile(path)
			if os.IsNotExist(err) || os.IsPermission(err) {
				continue
			}
			if err != nil {
				t.Errorf("%s: %v", path, err)
				failures++
				continue
			}
			if identifier, _ := dict.String("CFBundleIdentifier"); identifier == "" {
				t.Errorf("%s parsed but has no CFBundleIdentifier", path)
				failures++
				continue
			}
			parsed++
		}
	}
	if parsed == 0 {
		t.Skip("no application bundles on this machine")
	}
	t.Logf("parsed %d real bundles, %d failures", parsed, failures)
}
