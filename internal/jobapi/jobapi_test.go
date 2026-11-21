package jobapi

import "testing"

func goodSpec() Spec {
	return Spec{
		JobID:    "j1",
		Tenant:   "t",
		Repo:     "github.com/acme/api",
		Commit:   "abc",
		Hardware: NewHardware(ArchX86_64, 8),
		Image:    "ubuntu-24.04@2026-11-16",
		Trust:    Trusted,
	}
}

func TestSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		mut     func(*Spec)
		wantErr bool
	}{
		{"good", func(*Spec) {}, false},
		{"missing job id", func(s *Spec) { s.JobID = "" }, true},
		{"missing repo", func(s *Spec) { s.Repo = "" }, true},
		{"bad vcpu", func(s *Spec) { s.Hardware = Hardware{Arch: ArchX86_64, VCPU: 3, MemoryGB: 12} }, true},
		{"wrong memory", func(s *Spec) { s.Hardware.MemoryGB = 16 }, true},
		{"missing image", func(s *Spec) { s.Image = "" }, true},
		{"bad trust", func(s *Spec) { s.Trust = "maybe" }, true},
		{"empty trust", func(s *Spec) { s.Trust = "" }, true},
		{"fork trust ok", func(s *Spec) { s.Trust = UntrustedFork }, false},
		{"docker volume ok", func(s *Spec) { s.Volumes = []Volume{DockerVolume} }, false},
		{"volume on root", func(s *Spec) { s.Volumes = []Volume{{Kind: VolumeSticky, Key: "r", Mount: "/"}} }, true},
		{"relative mount", func(s *Spec) { s.Volumes = []Volume{{Kind: VolumeSticky, Key: "r", Mount: "var/x"}} }, true},
		{"unclean mount", func(s *Spec) { s.Volumes = []Volume{{Kind: VolumeSticky, Key: "r", Mount: "/var/../x"}} }, true},
		{"missing key", func(s *Spec) { s.Volumes = []Volume{{Kind: VolumeSticky, Mount: "/x"}} }, true},
		{"docker key elsewhere", func(s *Spec) {
			s.Volumes = []Volume{{Kind: VolumeSticky, Key: "docker", Mount: "/srv/docker"}}
		}, true},
		{"other key at docker mount ok", func(s *Spec) {
			s.Volumes = []Volume{{Kind: VolumeSticky, Key: "docker-big", Mount: "/var/lib/docker"}}
		}, false},
		{"duplicate key", func(s *Spec) {
			s.Volumes = []Volume{{Kind: VolumeSticky, Key: "c", Mount: "/x"}, {Kind: VolumeSticky, Key: "c", Mount: "/y"}}
		}, true},
		{"duplicate mount", func(s *Spec) {
			s.Volumes = []Volume{DockerVolume, {Kind: VolumeSticky, Key: "d2", Mount: "/var/lib/docker"}}
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := goodSpec()
			tt.mut(&s)
			if err := s.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidSizeAndHardware(t *testing.T) {
	for _, n := range []int{2, 4, 8, 16} {
		if !ValidSize(n) {
			t.Errorf("ValidSize(%d) = false", n)
		}
	}
	for _, n := range []int{0, 1, 3, 32} {
		if ValidSize(n) {
			t.Errorf("ValidSize(%d) = true", n)
		}
	}
	if h := NewHardware(ArchARM64, 4); h.MemoryGB != 16 || h.Arch != ArchARM64 {
		t.Errorf("NewHardware = %+v", h)
	}
}

func TestWithDockerVolume(t *testing.T) {
	cache := Volume{Kind: VolumeSticky, Key: "cache", Mount: "/home/runner/.cache"}

	got := WithDockerVolume(nil)
	if len(got) != 1 || got[0] != DockerVolume {
		t.Fatalf("nil -> %v", got)
	}
	in := []Volume{cache}
	got = WithDockerVolume(in)
	if len(got) != 2 || got[0] != cache || got[1] != DockerVolume {
		t.Fatalf("cache -> %v", got)
	}
	if len(in) != 1 {
		t.Fatalf("input modified: %v", in)
	}
	// An existing volume at /var/lib/docker (any key) is kept as is.
	own := []Volume{{Kind: VolumeSticky, Key: "docker-big", Mount: "/var/lib/docker/"}}
	if got = WithDockerVolume(own); len(got) != 1 || got[0].Key != "docker-big" {
		t.Fatalf("own -> %v", got)
	}
}
