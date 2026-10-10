package supervisor

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// GuardCommand is a private agent mode. It installs inherited process-group
// restrictions before replacing itself with the requested executable. FD 3 is
// a close-on-exec startup-status pipe; commands never receive that descriptor.
// Requests, including argv and environment, travel through stdin, not argv.
func GuardCommand() error {
	status := os.NewFile(3, "guard-status")
	if status == nil {
		return errors.New("command guard status pipe is missing")
	}
	defer func() { _ = status.Close() }()
	fail := func(message string) error {
		_, _ = io.WriteString(status, message)
		return errors.New(message)
	}
	unix.CloseOnExec(3)
	var req Request
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&req); err != nil || len(req.Argv) == 0 || req.Argv[0] == "" {
		return fail("invalid guarded command")
	}
	// no_new_privs and exec are thread-local; do not migrate between them.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := lockProcessGroup(); err != nil {
		return fail("command isolation unavailable")
	}
	executable, err := exec.LookPath(req.Argv[0])
	if err != nil {
		return fail("command failed to start")
	}
	// Commands do not have interactive stdin. Do not expose the request body.
	null, err := os.Open(os.DevNull)
	if err != nil {
		return fail("command failed to start")
	}
	defer func() { _ = null.Close() }()
	if err := unix.Dup2(int(null.Fd()), 0); err != nil {
		return fail("command failed to start")
	}
	if err := syscall.Exec(executable, req.Argv, mergeEnv(os.Environ(), req.Env)); err != nil {
		return fail("command failed to start")
	}
	return nil
}

func lockProcessGroup() error {
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	default:
		return errors.New("unsupported command architecture")
	}
	// Validate the syscall ABI before comparing syscall numbers. x32 uses the
	// x86_64 audit arch with a distinct syscall-number bit and is also denied.
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: arch, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	if runtime.GOARCH == "amd64" {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS})
	}
	for _, number := range []uint32{unix.SYS_SETSID, unix.SYS_SETPGID, unix.SYS_UNSHARE, unix.SYS_SETNS, unix.SYS_PTRACE, unix.SYS_PROCESS_VM_WRITEV} {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: number, Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)})
	}
	// clone3 arguments are behind a pointer and cannot be inspected by classic
	// BPF. ENOSYS enables the ordinary clone fallback used by libc/Go runtimes.
	filter = append(filter,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_CLONE3, Jf: 1},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_CLONE, Jf: 3},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: unix.CLONE_NEWUSER | unix.CLONE_NEWPID, Jf: 1},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	// Sync all Go runtime threads; exec must not select an unfiltered thread.
	result, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		return errno
	}
	if result != 0 {
		return errors.New("command filter thread synchronization failed")
	}
	return nil
}
