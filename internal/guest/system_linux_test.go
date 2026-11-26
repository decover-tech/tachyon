//go:build linux

package guest

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseMountinfo(t *testing.T) {
	in := "22 1 0:21 / /run rw,nosuid shared:5 - tmpfs tmpfs rw\n" +
		"40 22 254:16 / /run/tachyon/job rw,relatime shared:20 - ext4 /dev/vdb rw\n" +
		"41 22 0:40 / /run/tachyon/root rw - overlay overlay rw,lowerdir=/run/tachyon/base\n" +
		"42 41 254:32 / /run/tachyon/root/var/lib/my\\040docker rw - ext4 /dev/vdc rw\n" +
		"bad line\n"
	want := []mountEntry{
		{"tmpfs", "/run"},
		{"/dev/vdb", "/run/tachyon/job"},
		{"overlay", "/run/tachyon/root"},
		{"/dev/vdc", "/run/tachyon/root/var/lib/my docker"},
	}
	if got := parseMountinfo(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUnderDir(t *testing.T) {
	for p, want := range map[string]bool{
		"/run/tachyon/root":         true,
		"/run/tachyon/root/var/lib": true,
		"/run/tachyon/rootfs":       false,
		"/":                         false,
	} {
		if got := underDir(p, "/run/tachyon/root"); got != want {
			t.Errorf("underDir(%q) = %v", p, got)
		}
	}
}

func TestRunReturnsWhileBackgroundProcessHoldsOutput(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chroot and set credentials")
	}
	var out bytes.Buffer
	start := time.Now()
	code, err := (&LinuxSystem{}).Run(context.Background(), Command{
		Path:   "/bin/sh",
		Args:   []string{"-c", "sleep 30 & echo started"},
		Root:   "/",
		Dir:    "/",
		Stdout: &out,
		Stderr: &out,
	})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v", code, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Run waited %s for the background process", d)
	}
	if !strings.Contains(out.String(), "started") {
		t.Errorf("output %q", out.String())
	}
}
