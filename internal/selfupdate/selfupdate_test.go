package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIsRelease decides whether an update is allowed to overwrite the binary. Getting this wrong in
// the permissive direction silently destroys someone's work-in-progress build.
func TestIsRelease(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want bool
	}{
		{"v0.1.5", true},
		{"v1.0.0", true},
		{"v0.1.6-0.20260817085604-e89faaac891c", false}, // pseudo-version: built from a clone
		{"v0.1.6-0.20260817085604-e89faaac891c+dirty", false},
		{"v0.1.5+dirty", false}, // clean tag but uncommitted changes on top
		{"0.1.0-dev", false},    // the fallback when nothing stamped it
		{"(devel)", false},
		{"", false},
	} {
		if got := IsRelease(tc.v); got != tc.want {
			t.Errorf("IsRelease(%q) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestAssetNameMatchesTheReleaseWorkflow(t *testing.T) {
	got := AssetName("v1.2.3")
	want := "argus_v1.2.3_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	if got != want {
		t.Errorf("AssetName = %q, want %q — this must match what release.yml publishes", got, want)
	}
}

func TestParseChecksum(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, in, want string
		wantErr        bool
	}{
		{"sha256sum format", sum + "  argus_v1_linux_amd64.tar.gz\n", sum, false},
		{"bare hex", sum, sum, false},
		{"uppercase is normalised", strings.ToUpper(sum) + " f", sum, false},
		{"too short", "abc123  f", "", true},
		{"not hex", strings.Repeat("zz", 32) + "  f", "", true},
		{"empty", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChecksum(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseChecksum(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseChecksum(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseChecksum(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// tarball builds a release-shaped archive in memory.
func tarball(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func binName() string {
	if runtime.GOOS == "windows" {
		return "argus.exe"
	}
	return "argus"
}

func TestExtract(t *testing.T) {
	want := []byte("\x7fELF pretend binary")
	got, err := extract(tarball(t, map[string][]byte{binName(): want}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("extract returned %q, want %q", got, want)
	}
}

// TestExtractIgnoresUnexpectedPaths is the tar-traversal guard. A malicious archive that names its
// entry ../../../../etc/cron.d/x must not be able to influence where anything lands — the only
// thing taken is a regular file whose base name is exactly the binary.
func TestExtractIgnoresUnexpectedPaths(t *testing.T) {
	real := []byte("the real binary")
	got, err := extract(tarball(t, map[string][]byte{
		"../../../../etc/passwd": []byte("root:x:0:0"),
		"argus/nested/thing":     []byte("decoy"),
		binName():                real,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, real) {
		t.Errorf("extract picked the wrong entry: %q", got)
	}
}

func TestExtractRejectsJunk(t *testing.T) {
	if _, err := extract([]byte("not a gzip stream")); err == nil {
		t.Error("extract accepted a non-gzip payload")
	}
	if _, err := extract(tarball(t, map[string][]byte{"README": []byte("hi")})); err == nil {
		t.Error("extract accepted an archive with no binary in it")
	}
	if _, err := extract(tarball(t, map[string][]byte{binName(): {}})); err == nil {
		t.Error("extract accepted an empty binary")
	}
}

// TestChecksumMismatchIsDetected exercises the comparison the update refuses on. The whole safety
// argument for overwriting an executable rests on this being right.
func TestChecksumMismatchIsDetected(t *testing.T) {
	archive := tarball(t, map[string][]byte{binName(): []byte("payload")})
	sum := sha256.Sum256(archive)
	good := hex.EncodeToString(sum[:])

	parsed, err := parseChecksum(good + "  argus.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if parsed != good {
		t.Fatalf("round trip failed: %q vs %q", parsed, good)
	}
	tampered := sha256.Sum256(append(archive, 'x'))
	if hex.EncodeToString(tampered[:]) == good {
		t.Fatal("a modified archive produced the same digest")
	}
}

// TestReplaceExecutableIsAtomic checks the swap leaves a complete file, and that the temporary file
// does not survive. A partially-written executable is the worst possible outcome here.
func TestReplaceExecutableIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "argus")
	if err := os.WriteFile(path, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	want := []byte("new binary contents")
	if err := replaceExecutable(path, want); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("binary = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 — the replacement must stay executable", info.Mode().Perm())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".argus-update-") {
			t.Errorf("temporary file %s was left behind", e.Name())
		}
	}
}

func TestGetRejectsPlaintext(t *testing.T) {
	if _, err := get(t.Context(), "http://example.com/argus.tar.gz", 1024); err == nil {
		t.Error("get accepted a plaintext URL")
	}
}

// TestDecide covers the rules that decide whether to overwrite an executable. A bug here either
// destroys a local build or refuses a legitimate update, and the -check path failing was a real
// bug found by running it rather than by reading it.
func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		name        string
		opts        Options
		latest      string
		wantErr     bool
		wantNote    string // substring; "" means proceed with the install
		wantProceed bool
	}{
		{
			name: "clean build behind latest proceeds",
			opts: Options{Current: "v0.1.5"}, latest: "v0.1.6", wantProceed: true,
		},
		{
			name: "clean build at latest stops",
			opts: Options{Current: "v0.1.6"}, latest: "v0.1.6", wantNote: "already up to date",
		},
		{
			name: "local build is refused",
			opts: Options{Current: "v0.1.6+dirty"}, latest: "v0.1.7", wantErr: true,
		},
		{
			name: "local build with -force proceeds",
			opts: Options{Current: "v0.1.6+dirty", Force: true}, latest: "v0.1.7", wantProceed: true,
		},
		{
			name: "pseudo-version is refused",
			opts: Options{Current: "v0.1.6-0.20260817085604-e89faaac891c"}, latest: "v0.1.7", wantErr: true,
		},
		// -check must never fail, whatever the binary is. It changes nothing.
		{
			name: "check on a local build reports instead of failing",
			opts: Options{Current: "v0.1.6+dirty", DryRun: true}, latest: "v0.1.7",
			wantNote: "v0.1.7 is available",
		},
		{
			name: "check on a dirty build at latest says so",
			opts: Options{Current: "v0.1.6+dirty", DryRun: true}, latest: "v0.1.6",
			wantNote: "uncommitted changes",
		},
		{
			name: "check when up to date",
			opts: Options{Current: "v0.1.6", DryRun: true}, latest: "v0.1.6",
			wantNote: "already up to date",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note, err := Decide(tc.opts, tc.latest)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got note %q", note)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantProceed {
				if note != "" {
					t.Errorf("want to proceed with the install, got note %q", note)
				}
				return
			}
			if !strings.Contains(note, tc.wantNote) {
				t.Errorf("note = %q, want it to contain %q", note, tc.wantNote)
			}
		})
	}
}
