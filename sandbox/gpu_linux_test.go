package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// With the GPU allowed a command can open a render node and talk to its
// driver; the nodes that drive a display stay closed, and without the
// setting neither kind opens.
func TestSandboxedGPURenderNodes(t *testing.T) {
	requireBackend(t)
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	nodes, _ := filepath.Glob("/dev/dri/renderD*")
	if len(nodes) == 0 {
		t.Skip("no GPU render node here")
	}
	if f, err := os.OpenFile(nodes[0], os.O_RDWR, 0); err != nil {
		t.Skipf("the render node does not open even without the sandbox: %v", err)
	} else {
		f.Close()
	}
	// DRM_IOCTL_VERSION, which every driver answers: the open alone would
	// not show that device controls get through.
	probe := py + ` -c "
import fcntl, glob, os
for path in sorted(glob.glob('/dev/dri/renderD*'))[:1] + sorted(glob.glob('/dev/dri/card*'))[:1]:
    kind = 'render' if 'renderD' in path else 'card'
    try:
        fd = os.open(path, os.O_RDWR)
    except OSError:
        print(kind, 'closed')
        continue
    try:
        fcntl.ioctl(fd, 0xC0406400, bytearray(64))
        print(kind, 'open')
    except OSError:
        print(kind, 'open, controls refused')
"`
	p, project, _, _, _ := fixture(t)
	if out, _ := sh(t, p, project, probe); !strings.Contains(out, "render closed") {
		t.Errorf("a render node opened without the setting:\n%s", out)
	}
	p.GPU = true
	out, _ := sh(t, p, project, probe)
	if !strings.Contains(out, "render open") || strings.Contains(out, "refused") {
		t.Errorf("the render node is not usable with the GPU allowed:\n%s", out)
	}
	if strings.Contains(out, "card open") {
		t.Errorf("the GPU setting opened a display node:\n%s", out)
	}
}

func TestGPUDevicesLeaveOutDisplays(t *testing.T) {
	for _, pattern := range gpuDevicePatterns {
		for _, display := range []string{"/dev/dri/card0", "/dev/nvidia-modeset", "/dev/fb0", "/dev/tty0"} {
			if ok, _ := filepath.Match(pattern, display); ok {
				t.Errorf("%s matches %s, which drives a display", pattern, display)
			}
		}
	}
}
