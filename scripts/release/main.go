// Command release builds versioned, checksummed archives from a clean Git commit.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var platforms = []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}
var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)

type provenance struct {
	Version   string   `json:"version"`
	Commit    string   `json:"commit"`
	GoVersion string   `json:"go_version"`
	Platforms []string `json:"platforms"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/release VERSION OUTPUT [OS/ARCH ...]")
		os.Exit(2)
	}
	selected := os.Args[3:]
	if len(selected) == 0 {
		selected = platforms
	}
	root, err := command("", "git", "rev-parse", "--show-toplevel")
	if err == nil {
		err = buildRelease(root, os.Args[1], os.Args[2], selected)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func command(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func validate(version string, selected []string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("invalid version %q; use vMAJOR.MINOR.PATCH[-PRERELEASE]", version)
	}
	if len(selected) == 0 {
		return fmt.Errorf("select at least one platform")
	}
	seen := make(map[string]bool)
	for _, target := range selected {
		valid := false
		for _, supported := range platforms {
			if supported == target {
				valid = true
			}
		}
		if !valid || seen[target] {
			return fmt.Errorf("unsupported or duplicate platform %q", target)
		}
		seen[target] = true
	}
	return nil
}

func buildRelease(root, version, out string, selected []string) error {
	if err := validate(version, selected); err != nil {
		return err
	}
	status, err := command(root, "git", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("source tree is dirty; commit changes before building")
	}
	commit, err := command(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if err := os.Mkdir(out, 0755); err != nil {
		return fmt.Errorf("create new output directory: %w", err)
	}
	// A failed build must not leave an apparently complete release directory.
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(out)
		}
	}()
	workspace, err := os.MkdirTemp("", "ghx-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	info, err := json.MarshalIndent(provenance{version, commit, runtime.Version(), selected}, "", "  ")
	if err != nil {
		return err
	}
	info = append(info, '\n')
	if err := os.WriteFile(filepath.Join(out, "source.json"), info, 0644); err != nil {
		return err
	}
	var checksums bytes.Buffer
	for _, target := range selected {
		parts := strings.Split(target, "/")
		name := "ghx-" + parts[0] + "-" + parts[1]
		dir := filepath.Join(workspace, name)
		if err := os.Mkdir(dir, 0755); err != nil {
			return err
		}
		suffix := ""
		if parts[0] == "windows" {
			suffix = ".exe"
		}
		files := []string{"ghx" + suffix, "ghxd" + suffix, "LICENSE", "source.json"}
		for _, binary := range []string{"ghx", "ghxd"} {
			cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=true", "-ldflags", "-s -w -X main.version="+version, "-o", filepath.Join(dir, binary+suffix), "./src/cmd/"+binary)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+parts[0], "GOARCH="+parts[1])
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("build %s %s: %w: %s", binary, target, err, output)
			}
		}
		license, err := os.ReadFile(filepath.Join(root, "LICENSE"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "LICENSE"), license, 0644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "source.json"), info, 0644); err != nil {
			return err
		}
		shim := "gh"
		shimBytes := "#!/bin/sh\n# ghx-shim: routes gh commands through ghx\nexec ghx \"$@\"\n"
		if parts[0] == "windows" {
			shim = "gh.cmd"
			shimBytes = "@echo off\r\nrem ghx-shim: routes gh commands through ghx\r\nghx %*\r\n"
		}
		if err := os.WriteFile(filepath.Join(dir, shim), []byte(shimBytes), 0755); err != nil {
			return err
		}
		files = append(files, shim)
		if parts[0] == "linux" {
			unit, err := os.ReadFile(filepath.Join(root, "contrib/systemd/ghxd.service"))
			if err == nil {
				if err := os.WriteFile(filepath.Join(dir, "ghxd.service"), unit, 0644); err != nil {
					return err
				}
				files = append(files, "ghxd.service")
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		archive := name + ".tar.gz"
		if parts[0] == "windows" {
			archive = name + ".zip"
		}
		if err := archiveFiles(dir, filepath.Join(out, archive), files, parts[0] == "windows"); err != nil {
			return err
		}
		if err := checksum(&checksums, out, archive); err != nil {
			return err
		}
	}
	if err := checksum(&checksums, out, "source.json"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "checksums.txt"), checksums.Bytes(), 0644); err != nil {
		return err
	}
	status, err = command(root, "git", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	head, err := command(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if status != "" || head != commit {
		return fmt.Errorf("source changed during build; rebuild from a clean commit")
	}
	complete = true
	return nil
}

func checksum(w io.Writer, dir, name string) error {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%x  %s\n", h.Sum(nil), name)
	return err
}

func archiveFiles(dir, path string, files []string, windows bool) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if e := f.Close(); err == nil {
			err = e
		}
	}()
	var zw *zip.Writer
	var tw *tar.Writer
	if windows {
		zw = zip.NewWriter(f)
		defer func() {
			if e := zw.Close(); err == nil {
				err = e
			}
		}()
	} else {
		gz := gzip.NewWriter(f)
		defer func() {
			if e := gz.Close(); err == nil {
				err = e
			}
		}()
		tw = tar.NewWriter(gz)
		defer func() {
			if e := tw.Close(); err == nil {
				err = e
			}
		}()
	}
	for _, name := range files {
		data, e := os.ReadFile(filepath.Join(dir, name))
		if e != nil {
			return e
		}
		stat, e := os.Stat(filepath.Join(dir, name))
		if e != nil {
			return e
		}
		var w io.Writer
		if windows {
			h := &zip.FileHeader{Name: name, Method: zip.Deflate}
			h.SetMode(stat.Mode())
			w, e = zw.CreateHeader(h)
		} else {
			e = tw.WriteHeader(&tar.Header{Name: name, Mode: int64(stat.Mode().Perm()), Size: int64(len(data))})
			w = tw
		}
		if e != nil {
			return e
		}
		if _, e := w.Write(data); e != nil {
			return e
		}
	}
	return nil
}
