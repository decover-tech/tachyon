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
