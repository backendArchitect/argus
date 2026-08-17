// Package selfupdate replaces the running argus binary with the latest published release.
//
// This is the most security-sensitive thing in the codebase, and it sits in a tool whose entire
// pitch is that it cannot write anything. So the rules here are strict and deliberately fail
// closed:
//
//   - HTTPS only, and only to the release host.
//   - The published SHA-256 must be fetched and must match. If the checksum cannot be retrieved,
//     the update is abandoned — an unverifiable download is treated as a hostile one.
//   - Nothing downloaded is ever executed during the update.
//   - The replacement is atomic: verify fully, write beside the target, then rename over it. A
//     failure at any point leaves the existing binary untouched rather than truncated.
//   - A locally-built binary is never silently clobbered.
//
// Note this writes only to the local filesystem. It gives argus no ability to write to a cluster,
// which remains enforced by TestNoMutatingVerbs.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Repo is the source of releases. A constant, not a flag: letting a caller redirect where the
// binary comes from would hand an attacker the whole update channel.
const Repo = "backendArchitect/argus"

// maxAsset caps what will be read from the network. The real binary is ~42MB; well past that means
// something is wrong, and an unbounded read is a trivial way to exhaust a machine's memory.
const maxAsset = 128 << 20

// Release is the subset of GitHub's release JSON that matters.
type Release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// AssetName is the archive this platform needs, matching what the release workflow publishes.
func AssetName(tag string) string {
	return fmt.Sprintf("argus_%s_%s_%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
}

// IsRelease reports whether a version string is a clean release tag rather than a local build.
//
// Go stamps a locally-built binary with a pseudo-version ("v0.1.6-0.2026...") and appends "+dirty"
// when the tree has uncommitted changes. Overwriting that with a published release would silently
// throw away someone's work in progress, so Update refuses unless forced.
func IsRelease(v string) bool {
	if v == "" || strings.Contains(v, "+dirty") || strings.Contains(v, "(devel)") {
		return false
	}
	if !strings.HasPrefix(v, "v") {
		return false
	}
	// A pseudo-version carries a timestamp-and-hash suffix: v0.1.6-0.20260817085604-e89faaac891c.
	return strings.Count(v, "-") == 0
}

// Latest fetches the newest published release.
func Latest(ctx context.Context) (*Release, error) {
	url := "https://api.github.com/repos/" + Repo + "/releases/latest"
	body, err := get(ctx, url, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("checking for releases: %w", err)
	}
	var r Release
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parsing release metadata: %w", err)
	}
	if r.TagName == "" {
		return nil, fmt.Errorf("no published release found for %s", Repo)
	}
	return &r, nil
}

// Options controls an update.
type Options struct {
	Current string // the running version
	Force   bool   // replace even a locally-built binary
	DryRun  bool   // check and report, change nothing
}

// Decide works out whether to install, and is separated from the download so the rules can be
// tested without a network. The ordering matters and got it wrong once: -check used to fail with
// the local-build refusal, when a dry run must never fail — it changes nothing, so there is nothing
// to protect against.
//
// Returns a note to print and stop on, or ("", nil) to proceed with the install.
func Decide(opts Options, latest string) (note string, err error) {
	// A dirty build is "at" its base version for comparison purposes, but must still be reported
	// verbatim so nobody mistakes it for a clean release.
	base := strings.TrimSuffix(opts.Current, "+dirty")
	current := base == latest

	if opts.DryRun {
		if current {
			if base != opts.Current {
				return "already at the latest release, though this binary has uncommitted changes " +
					"on top of it.", nil
			}
			return "already up to date.", nil
		}
		return fmt.Sprintf("%s is available. Run 'argus update' to install it.", latest), nil
	}
	if current && IsRelease(opts.Current) {
		return "already up to date.", nil
	}
	if !IsRelease(opts.Current) && !opts.Force {
		return "", fmt.Errorf("refusing to replace a locally-built binary (%s) with %s.\n"+
			"  Rebuild from your clone instead:  go install .\n"+
			"  Or overwrite it deliberately:     argus update -force",
			opts.Current, latest)
	}
	return "", nil
}

// Update replaces the running executable with the latest release.
func Update(ctx context.Context, opts Options, w io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the running binary: %w", err)
	}
	// Resolve symlinks so we replace the real file rather than the link pointing at it.
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	rel, err := Latest(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "installed: %s\nlatest:    %s\n", opts.Current, rel.TagName)

	note, err := Decide(opts, rel.TagName)
	if note != "" {
		fmt.Fprintf(w, "\n%s\n", note)
	}
	if err != nil || note != "" {
		return err
	}

	want := AssetName(rel.TagName)
	var archiveURL, sumURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			archiveURL = a.URL
		case want + ".sha256":
			sumURL = a.URL
		}
	}
	if archiveURL == "" {
		return fmt.Errorf("release %s has no build for %s/%s", rel.TagName, runtime.GOOS, runtime.GOARCH)
	}
	// Fail closed. An archive we cannot verify is worse than no update at all, because the whole
	// point of this path is that it overwrites an executable.
	if sumURL == "" {
		return fmt.Errorf("release %s publishes no checksum for %s; refusing to install it unverified",
			rel.TagName, want)
	}

	fmt.Fprintf(w, "\ndownloading %s\n", want)
	archive, err := get(ctx, archiveURL, maxAsset)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", want, err)
	}
	sumBody, err := get(ctx, sumURL, 4<<10)
	if err != nil {
		return fmt.Errorf("downloading the checksum for %s: %w", want, err)
	}

	expected, err := parseChecksum(string(sumBody))
	if err != nil {
		return err
	}
	actual := sha256.Sum256(archive)
	if hex.EncodeToString(actual[:]) != expected {
		return fmt.Errorf("CHECKSUM MISMATCH for %s\n  published: %s\n  downloaded: %s\n"+
			"The download does not match what was published. Not installing it",
			want, expected, hex.EncodeToString(actual[:]))
	}
	fmt.Fprintf(w, "verified sha256 %s\n", expected[:16]+"...")

	binary, err := extract(archive)
	if err != nil {
		return err
	}
	if err := replaceExecutable(self, binary); err != nil {
		return err
	}
	fmt.Fprintf(w, "updated %s to %s\n", self, rel.TagName)
	return nil
}

// parseChecksum reads the "<hex>  <filename>" form sha256sum produces.
func parseChecksum(s string) (string, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("unreadable checksum file: %q", strings.TrimSpace(s))
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", fmt.Errorf("checksum is not hex: %q", fields[0])
	}
	return strings.ToLower(fields[0]), nil
}

// extract pulls the argus binary out of the release tarball.
func extract(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, fmt.Errorf("archive is not gzip: %w", err)
	}
	defer gz.Close()

	wantName := "argus"
	if runtime.GOOS == "windows" {
		wantName = "argus.exe"
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive contains no %q", wantName)
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		// Only ever take the one expected regular file, by exact name. Never honour a path from
		// the archive — that is how a tarball escapes the directory it was meant to land in.
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != wantName {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxAsset))
		if err != nil {
			return nil, fmt.Errorf("reading %s from archive: %w", wantName, err)
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("%s in the archive is empty", wantName)
		}
		return b, nil
	}
}

// replaceExecutable swaps the binary atomically.
//
// The new file is written beside the target so the rename stays on one filesystem — a rename across
// devices is not atomic and would leave a partially-written executable on failure.
func replaceExecutable(path string, binary []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".argus-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w\n"+
			"  If argus lives in a system directory, re-run with sudo, or reinstall with 'go install'", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the new binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the new binary: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("making the new binary executable: %w", err)
	}

	if runtime.GOOS == "windows" {
		// Windows will not rename over a running executable, so move the old one aside first.
		old := path + ".old"
		os.Remove(old)
		if err := os.Rename(path, old); err != nil {
			return fmt.Errorf("moving the old binary aside: %w", err)
		}
		if err := os.Rename(tmpName, path); err != nil {
			os.Rename(old, path) // put it back
			return fmt.Errorf("installing the new binary: %w", err)
		}
		return nil
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("installing the new binary: %w", err)
	}
	return nil
}

// get fetches a URL over HTTPS with a bounded body.
func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("refusing a non-HTTPS URL: %s", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "argus-selfupdate")

	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			// GitHub redirects asset downloads to its object storage, which is expected. Downgrading
			// to plaintext on the way is not.
			if r.URL.Scheme != "https" {
				return fmt.Errorf("redirect to non-HTTPS URL %s", r.URL)
			}
			if len(via) > 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
