// tachyon is the developer CLI.
//
//	tachyon prebuild validate [repo-dir]
//	tachyon prebuild key -image ubuntu-24.04@2026-11-16 [repo-dir]
//	tachyon hostcert ca|host|server ...   (see hostcert.go)
//	tachyon github-app manifest -webhook-url https://cp.example/webhooks/github
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/decover-tech/tachyon/internal/frontend/github"
	"github.com/decover-tech/tachyon/internal/prebuild"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  tachyon prebuild validate [repo-dir]
  tachyon prebuild key -image <name@version> [repo-dir]
  tachyon hostcert ca -out <prefix> [-ttl 8760h]
  tachyon hostcert host -ca <prefix> -id <host_id> -out <prefix> [-ttl 720h] [-trust-domain tachyon]
  tachyon hostcert server -ca <prefix> -name <dns-or-ip>[,...] -out <prefix> [-ttl 2160h]
  tachyon github-app manifest -webhook-url <url> [-name Tachyon] [-homepage <url>] [-redirect-url <url>] [-public] [-features infra-rerun,nightly]`)
	os.Exit(2)
}

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "github-app" && os.Args[2] == "manifest" {
		appManifest(os.Args[3:])
		return
	}
	if len(os.Args) < 3 {
		usage()
	}
	switch os.Args[1] {
	case "prebuild":
	case "hostcert":
		hostcert(os.Args[2], os.Args[3:])
		return
	default:
		usage()
	}
	switch os.Args[2] {
	case "validate":
		dir := argDir(os.Args[3:])
		cfg, _, err := prebuild.Load(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s: ok (%d key files, %d paths, %d warm steps)\n", prebuild.ConfigPath, len(cfg.KeyFiles), len(cfg.Paths), len(cfg.Warm))
		if ws := cfg.WorkspacePaths(); len(ws) > 0 {
			fmt.Printf("workspace paths %v need the tachyon/warm step after actions/checkout\n", ws)
		}
	case "key":
		fs := flag.NewFlagSet("key", flag.ExitOnError)
		image := fs.String("image", "", "base image name@version")
		fs.Parse(os.Args[3:])
		if *image == "" {
			usage()
		}
		dir := argDir(fs.Args())
		cfg, raw, err := prebuild.Load(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		files, err := prebuild.ReadKeyFiles(dir, cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(prebuild.Key(*image, raw, files))
	default:
		usage()
	}
}

// appManifest prints the GitHub App manifest with Tachyon's least-privilege
// permissions and events. See docs/github-app.md.
func appManifest(args []string) {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	var o github.ManifestOptions
	fs.StringVar(&o.Name, "name", "Tachyon", "App name (unique on GitHub)")
	fs.StringVar(&o.Homepage, "homepage", "https://github.com/decover-tech/tachyon", "App homepage URL")
	fs.StringVar(&o.WebhookURL, "webhook-url", "", "control plane webhook URL, ending in /webhooks/github (https)")
	fs.StringVar(&o.RedirectURL, "redirect-url", "", "where GitHub redirects with the manifest code (optional)")
	fs.BoolVar(&o.Public, "public", false, "allow installs by accounts other than the owner")
	features := fs.String("features", "", "comma-separated opt-in features whose extra permissions the App requests: infra-rerun (Actions write), nightly (Actions write, Checks write)")
	fs.Parse(args)
	fl, err := github.ParseFeatures(strings.Split(*features, ","))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	o.Features = fl
	m, err := github.NewManifest(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func argDir(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return "."
}
