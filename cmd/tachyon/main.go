// tachyon is the developer CLI.
//
//	tachyon prebuild validate [repo-dir]
//	tachyon prebuild key -image ubuntu-24.04@2026-11-16 [repo-dir]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/decover-tech/tachyon/internal/prebuild"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  tachyon prebuild validate [repo-dir]
  tachyon prebuild key -image <name@version> [repo-dir]`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 3 || os.Args[1] != "prebuild" {
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

func argDir(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return "."
}
