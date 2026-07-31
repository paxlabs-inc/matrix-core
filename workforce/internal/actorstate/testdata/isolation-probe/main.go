package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"matrix/workforce/internal/actorstate"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/seatworker"
)

func main() {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, actorstate.MaxPacketBytes+1))
	if err != nil || len(input) > actorstate.MaxPacketBytes {
		fail("packet_read")
	}
	session, err := contracts.DecodeCanonical[
		seatworker.SessionInput, *seatworker.SessionInput,
	](input)
	if err != nil {
		fail("packet_decode")
	}
	packet := session.Packet
	checkEnvironment()
	checkIdentity()
	checkFilesystem(packet.Goal.Title)
	checkProc()
	checkNetwork()
	output, err := actorstate.Orient(packet)
	if err != nil {
		fail("packet_orientation")
	}
	encoded, err := contracts.EncodeCanonical(&output)
	if err != nil {
		fail("output_encode")
	}
	if _, err := os.Stdout.Write(encoded); err != nil {
		fail("output_write")
	}
}

func checkEnvironment() {
	allowed := map[string]bool{
		"WORKFORCE_SESSION": true,
		"PWD":               true,
	}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !allowed[key] {
			fail("ambient_environment_" + key)
		}
	}
	if os.Getenv("WORKFORCE_SESSION") != "1" {
		fail("session_environment")
	}
	if os.Getenv("PWD") != "/session" {
		fail("working_directory_environment")
	}
	for _, key := range []string{
		"AWS_SECRET_ACCESS_KEY", "DATABASE_URL", "ROUTER_ADMIN_TOKEN",
		"EFFECT_PROVIDER_TOKEN", "WORKFORCE_CAPABILITY_TOKEN", "HOME", "PATH",
	} {
		if os.Getenv(key) != "" {
			fail("credential_environment")
		}
	}
}

func checkIdentity() {
	if os.Getuid() != 65534 || os.Geteuid() != 65534 ||
		os.Getgid() != 65534 || os.Getegid() != 65534 {
		fail("privileged_identity")
	}
}

func checkFilesystem(hostMarker string) {
	workingDirectory, err := os.Getwd()
	if err != nil || workingDirectory != "/session" {
		fail("working_directory")
	}
	sessionFile := filepath.Join(workingDirectory, "probe")
	if err := os.WriteFile(sessionFile, []byte("session-only"), 0o600); err != nil {
		fail("session_write")
	}
	for _, path := range []string{hostMarker, "/root", "/home", "/etc/passwd", "/tmp"} {
		if _, err := os.Stat(path); err == nil {
			fail("host_filesystem")
		}
	}
	link := filepath.Join(workingDirectory, "host-link")
	if err := os.Symlink(hostMarker, link); err != nil {
		fail("session_symlink")
	}
	if _, err := os.ReadFile(link); err == nil {
		fail("symlink_escape")
	}
}

func checkProc() {
	status, err := os.Open("/proc/self/status")
	if err != nil {
		fail("proc_status")
	}
	defer status.Close()
	scanner := bufio.NewScanner(status)
	capabilityFields := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "CapEff:") || strings.HasPrefix(line, "CapBnd:") {
			_, raw, _ := strings.Cut(line, ":")
			value, parseErr := strconv.ParseUint(strings.TrimSpace(raw), 16, 64)
			if parseErr != nil || value != 0 {
				fail("linux_capabilities")
			}
			capabilityFields++
		}
	}
	if scanner.Err() != nil || capabilityFields != 2 {
		fail("proc_capabilities")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		fail("proc_directory")
	}
	processes := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err == nil {
			processes++
		}
	}
	if processes > 3 {
		fail("host_procfs")
	}
}

func checkNetwork() {
	connection, err := net.DialTimeout("tcp", "192.0.2.1:80", 250*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		fail("external_network")
	}
}

func fail(reason string) {
	_, _ = fmt.Fprintln(os.Stderr, reason)
	os.Exit(29)
}
