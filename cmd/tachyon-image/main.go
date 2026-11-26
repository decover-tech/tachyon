// tachyon-image is the base image pipeline's tool. images/<name>/build.sh
// calls it around the privileged build steps:
//
//	tachyon-image version                       this week's version (YYYY-MM-DD)
//	tachyon-image env      -config image.yml    image.yml as shell assignments
//	tachyon-image plan     -config image.yml -runner-images DIR -version V -apt-snapshot TS
//	tachyon-image sbom     -ref name@version -source-date-epoch N -dpkg FILE [-toolcache FILE]
//	tachyon-image manifest -config image.yml -dir DIR -version V -apt-snapshot TS -plan-sha256 H -source-date-epoch N
//	tachyon-image sign     -key key.pem manifest.json
//	tachyon-image verify   -trust keys.pem -dir DIR manifest.signed.json
//	tachyon-image diff     a/manifest.json b/manifest.json
//
// Output goes to stdout unless -o is given.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/decover-tech/tachyon/internal/images"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tachyon-image version|env|plan|sbom|manifest|sign|verify|diff [flags]")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "version":
		fmt.Println(images.WeeklyVersion(time.Now()))
	case "env":
		fs := flag.NewFlagSet("env", flag.ExitOnError)
		cfgPath := fs.String("config", "", "image.yml")
		fs.Parse(args)
		var cfg *images.Config
		if cfg, err = loadConfig(*cfgPath); err == nil {
			fmt.Print(cfg.ShellEnv())
		}
	case "plan":
		err = plan(args)
	case "sbom":
		err = sbom(args)
	case "manifest":
		err = manifest(args)
	case "sign":
		err = sign(args)
	case "verify":
		err = verify(args)
	case "diff":
		err = diff(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tachyon-image %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func output(fs *flag.FlagSet) *string { return fs.String("o", "", "output file (default stdout)") }

func write(path string, data []byte) error {
	if path == "" {
		_, err := os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func loadConfig(path string) (*images.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return images.ParseConfig(data)
}

func plan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	cfgPath := fs.String("config", "", "image.yml")
	ri := fs.String("runner-images", "", "actions/runner-images checkout at the pinned commit")
	version := fs.String("version", "", "image version")
	apt := fs.String("apt-snapshot", "", "snapshot.ubuntu.com timestamp, e.g. 20261116T000000Z")
	asJSON := fs.Bool("json", false, "print the plan as JSON instead of shell")
	out := output(fs)
	fs.Parse(args)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	tmpl, err := os.ReadFile(filepath.Join(*ri, cfg.RunnerImages.Template))
	if err != nil {
		return err
	}
	p, err := images.MakePlan(cfg, tmpl, images.BuildVars{ImageVersion: *version, APTSnapshot: *apt})
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(p, "", "  ")
		return write(*out, append(b, '\n'))
	}
	return write(*out, []byte(p.Shell()))
}

func sbom(args []string) error {
	fs := flag.NewFlagSet("sbom", flag.ExitOnError)
	refS := fs.String("ref", "", "name@version")
	sde := fs.Int64("source-date-epoch", 0, "SOURCE_DATE_EPOCH of the build")
	dpkg := fs.String("dpkg", "", "dpkg-query output (Package, Version, Architecture; tab-separated)")
	toolcache := fs.String("toolcache", "", "tool cache listing (name, version; tab-separated)")
	out := output(fs)
	fs.Parse(args)
	ref, err := images.ParseRef(*refS)
	if err != nil {
		return err
	}
	f, err := os.Open(*dpkg)
	if err != nil {
		return err
	}
	defer f.Close()
	pkgs, err := images.ParseDpkg(f)
	if err != nil {
		return err
	}
	if *toolcache != "" {
		tf, err := os.Open(*toolcache)
		if err != nil {
			return err
		}
		defer tf.Close()
		tools, err := images.ParseToolcache(tf)
		if err != nil {
			return err
		}
		pkgs = append(pkgs, tools...)
	}
	b, err := images.SBOM(ref, *sde, pkgs)
	if err != nil {
		return err
	}
	return write(*out, b)
}

func manifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	cfgPath := fs.String("config", "", "image.yml")
	dir := fs.String("dir", "", "image output directory")
	version := fs.String("version", "", "image version")
	apt := fs.String("apt-snapshot", "", "snapshot.ubuntu.com timestamp")
	planSHA := fs.String("plan-sha256", "", "digest of the build plan")
	sde := fs.Int64("source-date-epoch", 0, "SOURCE_DATE_EPOCH of the build")
	rootfs := fs.String("rootfs", "rootfs.ext4", "ext4 image, relative to -dir")
	kernel := fs.String("kernel", "", "guest kernel, relative to -dir (optional)")
	sbomPath := fs.String("sbom", "sbom.spdx.json", "SBOM, relative to -dir")
	out := output(fs)
	fs.Parse(args)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	m := &images.Manifest{
		Name:    cfg.Name,
		Version: *version,
		Arch:    cfg.Arch,
		Source: images.Source{
			RunnerImagesRepo:    cfg.RunnerImages.Repo,
			RunnerImagesCommit:  cfg.RunnerImages.Commit,
			Template:            cfg.RunnerImages.Template,
			BaseRootfsSHA256:    cfg.BaseRootfs.SHA256,
			ActionsRunnerSHA256: cfg.ActionsRunner.SHA256,
			APTSnapshot:         *apt,
			PlanSHA256:          *planSHA,
		},
		SourceDateEpoch: *sde,
		Sizes:           images.Sizes,
	}
	if m.Rootfs, err = images.HashFile(*dir, *rootfs); err != nil {
		return err
	}
	if m.SBOM, err = images.HashFile(*dir, *sbomPath); err != nil {
		return err
	}
	if *kernel != "" {
		k, err := images.HashFile(*dir, *kernel)
		if err != nil {
			return err
		}
		m.Kernel = &k
	}
	if err := m.Validate(); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return write(*out, append(b, '\n'))
}

func readManifest(path string) (*images.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m images.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "", "ed25519 private key (PKCS#8 PEM)")
	out := output(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	pem, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	key, err := images.ParsePrivateKey(pem)
	if err != nil {
		return err
	}
	m, err := readManifest(fs.Arg(0))
	if err != nil {
		return err
	}
	s, err := images.Sign(m, key)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return write(*out, append(b, '\n'))
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	trust := fs.String("trust", "", "trusted ed25519 public keys (PEM)")
	dir := fs.String("dir", "", "image directory; if set, every file is re-hashed")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	pem, err := os.ReadFile(*trust)
	if err != nil {
		return err
	}
	keys, err := images.ParsePublicKeys(pem)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var s images.Signed
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	m, err := s.Verify(keys)
	if err != nil {
		return err
	}
	if *dir != "" {
		if err := m.VerifyFiles(*dir); err != nil {
			return err
		}
	}
	fmt.Printf("%s: signature ok (key %s)\n", m.Ref(), s.KeyID)
	return nil
}

func diff(args []string) error {
	if len(args) != 2 {
		usage()
	}
	a, err := readManifest(args[0])
	if err != nil {
		return err
	}
	b, err := readManifest(args[1])
	if err != nil {
		return err
	}
	if d := images.Diff(a, b); len(d) > 0 {
		for _, line := range d {
			fmt.Println(line)
		}
		return fmt.Errorf("builds differ in %d field(s)", len(d))
	}
	fmt.Println("reproduced: builds are identical")
	return nil
}
