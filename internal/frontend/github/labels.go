package github

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

const (
	LabelPrefix     = "tachyon-"
	LabelClean      = "tachyon-clean"      // fork the base image, not the prebuild
	LabelDeferrable = "tachyon-deferrable" // discounted off-peak class
	defaultVCPU     = 2
)

// runner label: tachyon-<image>[-arm][-<N>x], e.g. tachyon-ubuntu-24.04-8x.
var runnerLabelRE = regexp.MustCompile(`^tachyon-(ubuntu-\d+\.\d+)(-arm)?(?:-(\d+)x)?$`)

// RunsOn is what a job's labels ask Tachyon for.
type RunsOn struct {
	Image      string
	Hardware   jobapi.Hardware
	Clean      bool
	Deferrable bool
}

// IsTachyonJob reports whether any label targets Tachyon.
func IsTachyonJob(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, LabelPrefix) {
			return true
		}
	}
	return false
}

// ParseLabels reads the runs-on labels of a workflow job. Exactly one runner
// label (tachyon-<image>...) is required; modifier labels are optional.
// Unknown tachyon- labels are an error so typos fail loudly instead of
// silently landing on a default VM.
func ParseLabels(labels []string) (RunsOn, error) {
	var r RunsOn
	found := false
	for _, l := range labels {
		switch {
		case !strings.HasPrefix(l, LabelPrefix):
			continue // e.g. "self-hosted", "linux"
		case l == LabelClean:
			r.Clean = true
		case l == LabelDeferrable:
			r.Deferrable = true
		default:
			m := runnerLabelRE.FindStringSubmatch(l)
			if m == nil {
				return RunsOn{}, fmt.Errorf("unknown Tachyon label %q", l)
			}
			if found {
				return RunsOn{}, fmt.Errorf("more than one Tachyon runner label")
			}
			found = true
			arch := jobapi.ArchX86_64
			if m[2] != "" {
				arch = jobapi.ArchARM64
			}
			vcpu := defaultVCPU
			if m[3] != "" {
				vcpu, _ = strconv.Atoi(m[3])
			}
			if !jobapi.ValidSize(vcpu) {
				return RunsOn{}, fmt.Errorf("label %q: unsupported size %dx (want one of %v)", l, vcpu, jobapi.VMSizes)
			}
			r.Image = m[1]
			r.Hardware = jobapi.NewHardware(arch, vcpu)
		}
	}
	if !found {
		return RunsOn{}, fmt.Errorf("no Tachyon runner label (want e.g. tachyon-ubuntu-24.04-8x)")
	}
	return r, nil
}
