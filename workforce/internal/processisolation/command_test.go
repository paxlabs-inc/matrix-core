package processisolation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandBuildsClosedDeterministicBoundary(t *testing.T) {
	content, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	command, err := Command(context.Background(), Spec{
		Bubblewrap:          "/usr/bin/bwrap",
		Binary:              "/bin/true",
		ExpectedBuildDigest: hex.EncodeToString(sum[:]),
		Target:              "/workforce-seat",
		Env: map[string]string{
			"WORKFORCE_SESSION": "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	joined := strings.Join(command.Args, " ")
	for _, required := range []string{
		"--unshare-all", "--disable-userns", "--cap-drop ALL",
		"--uid 65534", "--gid 65534", "--clearenv",
		"--tmpfs /session", "--proc /proc", "--dev /dev",
		"--ro-bind-fd 3 /workforce-seat",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("sandbox arguments omit %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "CAPABILITY_TOKEN") ||
		!strings.Contains(joined, "WORKFORCE_SESSION") {
		t.Fatalf("sandbox environment contains authority material: %s", joined)
	}
	if len(command.Env) != 1 || command.Env[0] != "PATH=/usr/bin:/bin" {
		t.Fatalf("bubblewrap launcher environment = %#v", command.Env)
	}
}

func TestCommandRejectsEnvironmentAndTargetInjection(t *testing.T) {
	for _, spec := range []Spec{
		{
			Bubblewrap: "/usr/bin/bwrap", Binary: "/bin/true",
			ExpectedBuildDigest: strings.Repeat("0", 64),
			Target:              "/bin/sh", Env: map[string]string{"WORKFORCE_SESSION": "1"},
		},
		{
			Bubblewrap: "/usr/bin/bwrap", Binary: "/bin/true",
			ExpectedBuildDigest: strings.Repeat("0", 64),
			Target:              "/workforce-seat-escape",
			Env:                 map[string]string{"WORKFORCE_SESSION": "1"},
		},
		{
			Bubblewrap: "/usr/bin/bwrap", Binary: "/bin/true",
			ExpectedBuildDigest: strings.Repeat("0", 64),
			Target:              "/workforce-seat",
			Env:                 map[string]string{"LD_PRELOAD": "/host/library"},
		},
		{
			Bubblewrap: "/usr/bin/bwrap", Binary: "/bin/true",
			ExpectedBuildDigest: strings.Repeat("0", 64),
			Target:              "/workforce-seat",
			Env:                 map[string]string{"WORKFORCE_SESSION": "1\x00escape"},
		},
	} {
		if _, err := Command(context.Background(), spec); err == nil {
			t.Fatalf("accepted unsafe sandbox spec %#v", spec)
		}
	}
}

func TestCommandRejectsSymlinkAndWritableWorker(t *testing.T) {
	content, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	directory := t.TempDir()
	symlink := directory + "/worker-link"
	if err := os.Symlink("/bin/true", symlink); err != nil {
		t.Fatal(err)
	}
	writable := directory + "/worker-writable"
	if err := os.WriteFile(writable, content, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o775); err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{symlink, writable} {
		if _, err := Command(context.Background(), Spec{
			Bubblewrap: "/usr/bin/bwrap", Binary: binary,
			ExpectedBuildDigest: digest, Target: "/workforce-seat",
			Env: map[string]string{"WORKFORCE_SESSION": "1"},
		}); err == nil {
			t.Fatalf("accepted replaceable worker %q", binary)
		}
	}
}

func TestCommandRejectsUntrustedLauncher(t *testing.T) {
	content, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	directory := t.TempDir()
	launcher := filepath.Join(directory, "bwrap")
	if err := os.WriteFile(launcher, content, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "bwrap-link")
	if err := os.Symlink("/usr/bin/bwrap", symlink); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(directory, "bwrap-writable")
	if err := os.WriteFile(writable, content, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	// The launcher runs before any sandbox argument applies, so a launcher
	// under a caller-writable directory must never be accepted.
	for _, bubblewrap := range []string{launcher, symlink, writable} {
		command, err := Command(context.Background(), Spec{
			Bubblewrap: bubblewrap, Binary: "/bin/true",
			ExpectedBuildDigest: digest, Target: "/workforce-seat",
			Env: map[string]string{"WORKFORCE_SESSION": "1"},
		})
		if err == nil {
			_ = command.Close()
			t.Fatalf("accepted replaceable bubblewrap launcher %q", bubblewrap)
		}
	}
}

func TestCommandExecutesAPrivateFrozenWorkerImage(t *testing.T) {
	content, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	directory := t.TempDir()
	worker := filepath.Join(directory, "worker")
	if err := os.WriteFile(worker, content, 0o755); err != nil {
		t.Fatal(err)
	}
	command, err := Command(context.Background(), Spec{
		Bubblewrap: "/usr/bin/bwrap", Binary: worker,
		ExpectedBuildDigest: hex.EncodeToString(sum[:]), Target: "/workforce-seat",
		Env: map[string]string{"WORKFORCE_SESSION": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer command.Close()
	if len(command.ExtraFiles) != 1 {
		t.Fatalf("extra files = %d", len(command.ExtraFiles))
	}
	image := command.ExtraFiles[0]
	imageInfo, err := image.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// The dispatched image is a private copy, not the deployed binary, and it
	// denies writers.
	if os.SameFile(imageInfo, workerInfo(t, worker)) ||
		imageInfo.Mode().Perm()&0o222 != 0 {
		t.Fatalf("private worker image = %v", imageInfo.Mode())
	}
	// Rewriting the deployed binary after verification must not change the
	// bytes the sandbox will execute.
	if err := os.Chmod(worker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := image.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	frozen := sha256.New()
	if _, err := io.Copy(frozen, image); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(frozen.Sum(nil)) != hex.EncodeToString(sum[:]) {
		t.Fatal("dispatched worker image changed after verification")
	}
	private := command.directory
	if err := command.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Close(); err != nil {
		t.Fatalf("repeated release: %v", err)
	}
	if _, err := os.Stat(private); !os.IsNotExist(err) {
		t.Fatalf("private worker image survived release: %v", err)
	}
}

func workerInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
