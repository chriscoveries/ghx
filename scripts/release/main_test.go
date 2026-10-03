package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRejectInvalidInputs(t *testing.T) {
	for _, version := range []string{"", "latest", "v1.2", "v1.2.3 -X main.version=wrong"} {
		if validate(version, []string{"linux/amd64"}) == nil {
			t.Fatalf("accepted %q", version)
		}
	}
	for _, selected := range [][]string{nil, {"other/amd64"}, {"linux/amd64", "linux/amd64"}} {
		if validate("v1.2.3", selected) == nil {
			t.Fatalf("accepted %v", selected)
		}
	}
}

// Package actual committed sources and execute the resulting native binaries.
func TestPackageNativeRelease(t *testing.T) {
	root, err := command("", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "source")
	if _, err := command("", "git", "clone", "--quiet", "--no-hardlinks", root, clone); err != nil {
		t.Fatal(err)
	}
	version := "v1.2.3-test"
	target := runtime.GOOS + "/" + runtime.GOARCH
	out := filepath.Join(t.TempDir(), "release")
	if err := buildRelease(clone, version, out, []string{target}); err != nil {
		t.Fatal(err)
	}
	var expected bytes.Buffer
	base := "ghx-" + strings.ReplaceAll(target, "/", "-")
	archive := base + ".tar.gz"
	if runtime.GOOS == "windows" {
		archive = base + ".zip"
	}
	for _, name := range []string{archive, "source.json"} {
		if err := checksum(&expected, out, name); err != nil {
			t.Fatal(err)
		}
	}
	checksums, err := os.ReadFile(filepath.Join(out, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(checksums, expected.Bytes()) {
		t.Fatal("published checksums do not cover archive and provenance")
	}
	var info provenance
	data, err := os.ReadFile(filepath.Join(out, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	commit, err := command(clone, "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if info.Commit != commit || info.Version != version || info.GoVersion != runtime.Version() || len(info.Platforms) != 1 || info.Platforms[0] != target {
		t.Fatalf("provenance = %+v", info)
	}
	files := readArchive(t, filepath.Join(out, archive))
	license, err := os.ReadFile(filepath.Join(clone, "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(files["LICENSE"], license) || !bytes.Equal(files["source.json"], data) {
		t.Fatal("archive omitted or altered license/provenance")
	}
	suffix, shim := "", "gh"
	if runtime.GOOS == "windows" {
		suffix, shim = ".exe", "gh.cmd"
	}
	if !bytes.Contains(files[shim], []byte("ghx-shim")) {
		t.Fatal("archive omitted marked shim")
	}
	for _, name := range []string{"ghx", "ghxd"} {
		path := filepath.Join(t.TempDir(), name+suffix)
		if err := os.WriteFile(path, files[name+suffix], 0755); err != nil {
			t.Fatal(err)
		}
		arg := "xversion"
		if name == "ghxd" {
			arg = "--version"
		}
		cmd := exec.Command(path, arg)
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "LOCALAPPDATA="+t.TempDir())
		got, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != name+" version "+version+"\n" {
			t.Fatalf("version = %q", got)
		}
	}
	if err := buildRelease(clone, version, out, []string{target}); err == nil {
		t.Fatal("overwrote existing release directory")
	}
	if err := os.WriteFile(filepath.Join(clone, "untracked.txt"), license, 0644); err != nil {
		t.Fatal(err)
	}
	if err := buildRelease(clone, version, filepath.Join(t.TempDir(), "dirty"), []string{target}); err == nil {
		t.Fatal("packaged uncommitted source")
	}
}

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	if strings.HasSuffix(path, ".zip") {
		z, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		defer z.Close()
		for _, f := range z.File {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			files[f.Name], err = io.ReadAll(r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		r := tar.NewReader(gz)
		for {
			h, err := r.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, "..") {
				t.Fatal("unsafe archive path")
			}
			if h.Name == "ghx" || h.Name == "ghxd" || h.Name == "gh" {
				if h.Mode&0111 == 0 {
					t.Fatalf("%s is not executable", h.Name)
				}
			}
			files[h.Name], err = io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return files
}
