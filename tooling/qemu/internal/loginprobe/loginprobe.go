// Package loginprobe is the service naru's QEMU tests run to see that a Node has no login path
// (ADR 0047): which TCP ports it listens on, which program that gives a login is in its file
// system, and whether any process reads a terminal.
package loginprobe

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// Programs are the names of what gives a login: a shell, a getty, a login and an SSH server.
var Programs = []string{"sh", "ash", "bash", "dash", "zsh", "busybox", "getty", "agetty", "mingetty", "login", "sshd", "dropbear"}

// Options say where the probe looks.
type Options struct {
	// Root is the file system searched for Programs and Proc the process file system. Skip holds the
	// directories under Root the search does not enter: the process file system, the devices, and the
	// data disk the tests stage their programs on.
	Root, Proc string
	Skip       []string
	// Pass numbers the report, so that a reader sees a later one than the one it has.
	Pass int
}

// Run reports on the Node through log lines named "loginprobe", each with a kind: listener (addr and
// port of a TCP socket in the listening state), program (path of a file or link named in Programs),
// tty-holder (pid, comm and path of a terminal a process other than PID 1 holds open) and reader
// (pid, tid, comm and path of a terminal a thread is blocked reading). A last line, kind=done,
// reports that the probe finished with the counts of each and its Pass.
func Run(opts Options, log *slog.Logger) error {
	found, err := listening(opts.Proc)
	if err != nil {
		return err
	}
	for _, l := range found {
		log.Info("loginprobe", "kind", "listener", "addr", l.Addr().String(), "port", l.Port())
	}
	programs, err := programsIn(opts.Root, opts.Skip)
	if err != nil {
		return err
	}
	for _, p := range programs {
		log.Info("loginprobe", "kind", "program", "path", p)
	}
	holders, readers, err := terminals(opts.Proc)
	if err != nil {
		return err
	}
	for _, h := range holders {
		log.Info("loginprobe", "kind", "tty-holder", "pid", h.pid, "comm", h.comm, "path", h.path)
	}
	for _, r := range readers {
		log.Info("loginprobe", "kind", "reader", "pid", r.pid, "tid", r.tid, "comm", r.comm, "path", r.path)
	}
	log.Info("loginprobe", "kind", "done", "listeners", len(found), "programs", len(programs), "tty-holders", len(holders), "readers", len(readers), "pass", opts.Pass)
	return nil
}

// listenState is the state of a socket that listens, in /proc/net/tcp.
const listenState = "0A"

// listening returns the address and port of each TCP socket in the listening state, IPv4 then
// IPv6.
func listening(proc string) ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	for _, name := range []string{"tcp", "tcp6"} {
		f, err := os.Open(filepath.Join(proc, "net", name))
		if errors.Is(err, fs.ErrNotExist) && name == "tcp6" {
			continue // a kernel without IPv6 has no such file
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // the header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 || fields[3] != listenState {
				continue
			}
			ap, err := parseSocket(fields[1])
			if err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("/proc/net/%s: %w", name, err)
			}
			out = append(out, ap)
		}
		err = sc.Err()
		_ = f.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseSocket parses the ADDR:PORT of /proc/net/tcp: the address as the 32-bit words the kernel
// holds it in, each printed in host byte order, and the port in hex.
func parseSocket(s string) (netip.AddrPort, error) {
	addr, port, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("%q is not ADDR:PORT", s)
	}
	raw, err := hex.DecodeString(addr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, fmt.Errorf("%q is not an address", addr)
	}
	for i := 0; i < len(raw); i += 4 {
		slices.Reverse(raw[i : i+4])
	}
	a, _ := netip.AddrFromSlice(raw)
	p, err := strconv.ParseUint(port, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%q is not a port", port)
	}
	return netip.AddrPortFrom(a.Unmap(), uint16(p)), nil
}

// programsIn returns the path of every entry under root, but not under a directory of skip, named
// in Programs. A link counts: it is a way in.
func programsIn(root string, skip []string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil // a file that went away is not one that was there
		}
		if d.IsDir() {
			if slices.Contains(skip, path) {
				return fs.SkipDir
			}
			return nil
		}
		if slices.Contains(Programs, d.Name()) {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

type holder struct {
	pid, comm, path string
}

type reader struct {
	pid, tid, comm, path string
}

// terminalPrefixes are the paths a terminal is opened as: the console, a virtual terminal, a serial
// port and a pseudo-terminal.
var terminalPrefixes = []string{"/dev/console", "/dev/tty", "/dev/pts/", "/dev/ptmx"}

func isTerminal(path string) bool {
	return slices.ContainsFunc(terminalPrefixes, func(p string) bool { return strings.HasPrefix(path, p) })
}

// readSyscalls are the numbers of the system calls that read a file descriptor, by architecture:
// read, pread64, readv, preadv and preadv2.
var readSyscalls = map[string][]int{
	"amd64": {0, 17, 19, 295, 327},
	"arm64": {63, 67, 65, 69, 286},
}

// terminals returns each process other than PID 1 that holds a terminal open, and each thread
// blocked in a read of a terminal. PID 1 holds the console the kernel opened for it, so only its
// reading counts.
func terminals(proc string) ([]holder, []reader, error) {
	pids, err := filepath.Glob(filepath.Join(proc, "[0-9]*"))
	if err != nil {
		return nil, nil, err
	}
	var holders []holder
	var readers []reader
	for _, dir := range pids {
		pid := filepath.Base(dir)
		comm := strings.TrimSpace(readFile(filepath.Join(dir, "comm")))
		terms := map[string]string{} // fd to path
		fds, _ := os.ReadDir(filepath.Join(dir, "fd"))
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
			if err == nil && isTerminal(target) {
				terms[fd.Name()] = target
			}
		}
		if pid != "1" {
			for _, path := range sortedValues(terms) {
				holders = append(holders, holder{pid, comm, path})
			}
		}
		tasks, _ := os.ReadDir(filepath.Join(dir, "task"))
		for _, task := range tasks {
			// "number arg0 ...", or "running" for a thread that is not in a system call.
			f := strings.Fields(readFile(filepath.Join(dir, "task", task.Name(), "syscall")))
			if len(f) < 2 {
				continue
			}
			n, err := strconv.Atoi(f[0])
			if err != nil || !slices.Contains(readSyscalls[runtime.GOARCH], n) {
				continue
			}
			fd, err := strconv.ParseUint(strings.TrimPrefix(f[1], "0x"), 16, 32)
			if err != nil {
				continue
			}
			if path, ok := terms[strconv.FormatUint(fd, 10)]; ok {
				readers = append(readers, reader{pid, task.Name(), comm, path})
			}
		}
	}
	return holders, readers, nil
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// readFile returns the content of a file that may be gone or unreadable, as "".
func readFile(path string) string {
	b, _ := os.ReadFile(path) //nolint:gosec // a path under the process file system
	return string(b)
}
