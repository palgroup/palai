package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// sandboxCommand is the argv-driven behaviour set the E09 Task 4 workspace shell suites run inside a
// real hardened OCI sandbox to prove SAN-002/003/004. It receives the exact argv the shell tool
// passed (proving argv-form, not shell-string parsing) and reports through stdout and the mounted
// /workspace. It returns the process exit code.
func sandboxCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "san: missing behaviour")
		return 2
	}
	switch args[0] {
	case "ok":
		// The healthy neighbour control: exit cleanly so a co-scheduled tenant is provably intact.
		fmt.Println("ok")
		return 0
	case "echo":
		// Prove argv is passed verbatim — each argument on its own line, no shell splitting.
		for _, a := range args[1:] {
			fmt.Println(a)
		}
		return 0
	case "whoami":
		// Prove the sandbox runs as the unprivileged uid, never root.
		fmt.Printf("uid=%d gid=%d\n", os.Getuid(), os.Getgid())
		return 0
	case "socket":
		// Prove no container-runtime socket is reachable (SAN-002): neither mounted nor dialable.
		present := pathExists("/var/run/docker.sock") || pathExists("/run/docker.sock")
		_, dialErr := net.DialTimeout("unix", "/var/run/docker.sock", 500*time.Millisecond)
		fmt.Printf("docker_socket_present=%v dial_ok=%v\n", present, dialErr == nil)
		return 0
	case "write":
		// Mutate the workspace so a persisted file proves the mount is writable through the tool.
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "san write <rel> <content>")
			return 2
		}
		if err := os.WriteFile(filepath.Join("/workspace", args[1]), []byte(args[2]), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write: %v\n", err)
			return 1
		}
		fmt.Println("wrote", args[1])
		return 0
	case "secret":
		// Emit secret-shaped tokens the tool must redact before display or return (SAN redaction).
		fmt.Println("provider key sk-live-SANFIXTURESECRET0123456789 and bearer abcdefgh12345678")
		fmt.Println("github token ghp_SANFIXTUREGITHUBTOKEN0123456789")
		return 0
	case "dial":
		// Attempt egress to the named host; the no-network sandbox denies it (SAN-004 enforcement).
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "san dial <host>")
			return 2
		}
		_, err := net.DialTimeout("tcp", args[1]+":80", 1*time.Second)
		fmt.Printf("dial_ok=%v\n", err == nil)
		return 0
	case "memhog":
		// Exhaust memory so a real memory cgroup bound OOM-kills the process (SAN-003). Touch each
		// page so the pages are resident, not just reserved.
		var blocks [][]byte
		for {
			block := make([]byte, 16<<20)
			for i := range block {
				block[i] = 1
			}
			blocks = append(blocks, block)
		}
	case "children":
		// Spawn a sleeping child tree, then block: on container teardown the whole group is killed,
		// so no child reaches its post-sleep marker (SAN-002 process-group kill). The parent records
		// its pgid to prove it started before being torn down.
		count := 3
		if len(args) > 1 {
			if n, err := strconv.Atoi(args[1]); err == nil {
				count = n
			}
		}
		for i := 0; i < count; i++ {
			_ = exec.Command("/engine", "san", "sleepchild", strconv.Itoa(i)).Start()
		}
		_ = os.WriteFile("/workspace/scratch/parent-started", []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(60 * time.Second)
		return 0
	case "sleepchild":
		// Sleep, then write a completion marker. If the group is killed first, the marker never
		// appears — the observable proof the descendant was terminated with the parent.
		idx := "0"
		if len(args) > 1 {
			idx = args[1]
		}
		time.Sleep(60 * time.Second)
		_ = os.WriteFile(filepath.Join("/workspace/scratch", "child-"+idx+"-done"), []byte("done"), 0o644)
		return 0
	case "diskhog":
		// Write until the workspace/disk bound stops it (best-effort — SAN-003 disk ceiling).
		f, err := os.Create("/workspace/scratch/hog")
		if err != nil {
			fmt.Fprintf(os.Stderr, "diskhog: %v\n", err)
			return 1
		}
		defer f.Close()
		chunk := make([]byte, 4<<20)
		for {
			if _, err := f.Write(chunk); err != nil {
				fmt.Fprintf(os.Stderr, "diskhog stopped: %v\n", err)
				return 1
			}
		}
	default:
		fmt.Fprintf(os.Stderr, "san: unknown behaviour %q\n", args[0])
		return 2
	}
}
