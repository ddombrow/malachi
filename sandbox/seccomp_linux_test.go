package sandbox

import (
	"encoding/binary"
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

// runFilter interprets a seccomp program over one system call, the way the
// kernel would, and returns its verdict.
func runFilter(t *testing.T, f []unix.SockFilter, arch, nr uint32, args ...uint64) uint32 {
	t.Helper()
	data := make([]byte, 64)
	binary.NativeEndian.PutUint32(data[0:], nr)
	binary.NativeEndian.PutUint32(data[4:], arch)
	for i, a := range args {
		binary.NativeEndian.PutUint64(data[16+8*i:], a)
	}
	var acc uint32
	for pc := 0; pc < len(f); pc++ {
		in := f[pc]
		switch in.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			acc = binary.NativeEndian.Uint32(data[in.K:])
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if acc == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			if acc >= in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case unix.BPF_RET | unix.BPF_K:
			return in.K
		default:
			t.Fatalf("instruction %d: unknown code %#x", pc, in.Code)
		}
	}
	t.Fatal("the program ran off its end")
	return 0
}

func TestSocketFilter(t *testing.T) {
	const (
		amd64   = unix.AUDIT_ARCH_X86_64
		i386    = unix.AUDIT_ARCH_I386
		arm     = unix.AUDIT_ARCH_ARM
		high    = uint64(1) << 32 // a bit the kernel ignores in an int argument
		socket  = unix.SYS_SOCKET
		uring   = unix.SYS_IO_URING_SETUP
		inet    = unix.AF_INET
		local   = unix.AF_UNIX
		netlink = unix.AF_NETLINK
	)
	type call struct {
		name string
		arch uint32
		nr   uint32
		args []uint64
	}
	calls := []call{
		{"inet", amd64, socket, []uint64{inet, unix.SOCK_STREAM, 0}},
		{"inet6", amd64, socket, []uint64{unix.AF_INET6, unix.SOCK_STREAM, 0}},
		{"bluetooth", amd64, socket, []uint64{unix.AF_BLUETOOTH, unix.SOCK_STREAM, 0}},
		{"unix", amd64, socket, []uint64{local, unix.SOCK_STREAM, 0}},
		{"unix, upper bits set", amd64, socket, []uint64{local | high, unix.SOCK_STREAM, 0}},
		{"netlink route", amd64, socket, []uint64{netlink, unix.SOCK_RAW, unix.NETLINK_ROUTE}},
		{"netlink usersock", amd64, socket, []uint64{netlink, unix.SOCK_RAW, unix.NETLINK_USERSOCK}},
		{"netlink usersock, upper bits set", amd64, socket, []uint64{netlink | high, unix.SOCK_RAW, unix.NETLINK_USERSOCK | high}},
		{"io_uring", amd64, uring, nil},
		{"another call", amd64, unix.SYS_OPENAT, nil},
		{"x32 socket", amd64, socket | x32Bit, []uint64{inet, unix.SOCK_STREAM, 0}},
		{"i386 socket", i386, i386Socket, []uint64{inet, unix.SOCK_STREAM, 0}},
		{"i386 socketcall", i386, i386Socketcall, nil},
		{"i386 io_uring", i386, i386IoUringSetup, nil},
		// 41 is socket on amd64 and something harmless on i386: a filter that
		// forgot the table would get this one wrong the other way.
		{"i386 call 41", i386, socket, []uint64{inet, unix.SOCK_STREAM, 0}},
		{"arm call", arm, 281, nil},
	}
	// The verdicts for each policy, in the order of calls.
	allow, deny, nosys, kill := uint32(retAllow), uint32(retEPERM), uint32(retNoSys), uint32(retKill)
	for _, c := range []struct {
		network, unixSockets bool
		want                 []uint32
	}{
		{false, false, []uint32{deny, deny, deny, deny, deny, allow, deny, deny, deny, allow, nosys, deny, deny, deny, allow, kill}},
		{false, true, []uint32{deny, deny, deny, allow, allow, allow, deny, deny, deny, allow, nosys, deny, deny, deny, allow, kill}},
		{true, false, []uint32{allow, allow, allow, deny, deny, allow, deny, deny, deny, allow, nosys, deny, deny, deny, allow, kill}},
		{true, true, []uint32{allow, allow, allow, allow, allow, allow, deny, deny, allow, allow, nosys, deny, deny, allow, allow, kill}},
	} {
		f := socketFilter(Policy{Enabled: true, Network: c.network, UnixSockets: c.unixSockets}, amd64)
		for i, call := range calls {
			if got := runFilter(t, f, call.arch, call.nr, call.args...); got != c.want[i] {
				t.Errorf("network=%v unixSockets=%v: %s: verdict %s, want %s",
					c.network, c.unixSockets, call.name, verdict(got), verdict(c.want[i]))
			}
		}
	}

	// An architecture with one call table: anything foreign is killed.
	f := socketFilter(Policy{Enabled: true}, unix.AUDIT_ARCH_AARCH64)
	if got := runFilter(t, f, unix.AUDIT_ARCH_AARCH64, socket, inet, unix.SOCK_STREAM, 0); got != deny {
		t.Errorf("arm64 socket: verdict %s, want %s", verdict(got), verdict(deny))
	}
	if got := runFilter(t, f, arm, socket, inet, unix.SOCK_STREAM, 0); got != kill {
		t.Errorf("32-bit arm call on arm64: verdict %s, want %s", verdict(got), verdict(kill))
	}
}

func verdict(action uint32) string {
	switch action {
	case retAllow:
		return "allow"
	case retEPERM:
		return "EPERM"
	case retNoSys:
		return "ENOSYS"
	case retKill:
		return "kill"
	}
	return fmt.Sprintf("%#x", action)
}
