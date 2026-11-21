package proxy

import (
	"path"
	"strings"
	"time"
)

// Public upstreams. Tests and staging override them via Upstreams.
const (
	NPMUpstream       = "https://registry.npmjs.org"
	PyPIUpstream      = "https://pypi.org"
	PyPIFilesUpstream = "https://files.pythonhosted.org"
	GoUpstream        = "https://proxy.golang.org"
	MavenUpstream     = "https://repo1.maven.org/maven2"
)

// NPM mirrors the public npm registry. Tarballs are immutable; packuments
// (package metadata) change on publish. npm rewrites tarball URLs that
// point at registry.npmjs.org to the configured registry by default
// (replace-registry-host=npmjs), so packuments need no rewriting.
func NPM(base string) *Upstream {
	return &Upstream{
		Name:       "npm",
		Base:       base,
		TTL:        5 * time.Minute,
		VaryAccept: true, // abbreviated vs full packuments
		Classify: func(p string) Policy {
			if i := strings.Index(p, "/-/"); i >= 0 {
				if strings.HasSuffix(p, ".tgz") && i > 0 {
					return Immutable
				}
				return NoStore // search, audit, login, ...
			}
			return Mutable
		},
	}
}

// PyPISimple mirrors the PEP 503/691 simple index. File links in the index
// are absolute files.pythonhosted.org URLs; they are rewritten to this
// mirror's /pypi/files/ so downloads hit the cache too.
func PyPISimple(base, filesBase string) *Upstream {
	return &Upstream{
		Name:       "pypi",
		Base:       base,
		TTL:        10 * time.Minute,
		VaryAccept: true, // HTML vs JSON index
		Classify: func(p string) Policy {
			if strings.HasPrefix(p, "/simple/") {
				return Mutable
			}
			return NoStore
		},
		Rewrite: func(b []byte, mirror string) []byte {
			return replaceAll(b, strings.TrimSuffix(filesBase, "/")+"/", mirror+"/pypi/files/")
		},
	}
}

// PyPIFiles mirrors distribution files. Their paths contain a content hash,
// so they never change.
func PyPIFiles(base string) *Upstream {
	return &Upstream{
		Name:     "pypi-files",
		Base:     base,
		Classify: func(string) Policy { return Immutable },
	}
}

// GoProxy mirrors a GOPROXY. Versioned .info/.mod/.zip files are immutable
// (checksummed by sum.golang.org); version lists and @latest move. sumdb
// lookups pass through uncached.
func GoProxy(base string) *Upstream {
	return &Upstream{
		Name: "go",
		Base: base,
		TTL:  time.Minute,
		Classify: func(p string) Policy {
			switch {
			case strings.HasPrefix(p, "/sumdb/"):
				return NoStore
			case strings.HasSuffix(p, "/@v/list"), strings.HasSuffix(p, "/@latest"):
				return Mutable
			case strings.Contains(p, "/@v/"):
				switch path.Ext(p) {
				case ".info", ".mod", ".zip":
					return Immutable
				}
			}
			return NoStore
		},
	}
}

// Maven mirrors Maven Central. Released artifacts are immutable; metadata
// and snapshots are not.
func Maven(base string) *Upstream {
	return &Upstream{
		Name: "maven",
		Base: base,
		TTL:  5 * time.Minute,
		Classify: func(p string) Policy {
			if strings.HasSuffix(p, "/") {
				return NoStore // directory listings
			}
			if strings.Contains(path.Base(p), "maven-metadata.xml") || strings.Contains(p, "-SNAPSHOT") {
				return Mutable
			}
			return Immutable
		},
	}
}

// AptPolicy classifies apt repository paths: pool/ packages and by-hash
// indexes are immutable; other dists/ files (InRelease, Packages) move.
func AptPolicy(p string) Policy {
	switch {
	case strings.Contains(p, "/pool/"), strings.Contains(p, "/by-hash/"):
		return Immutable
	case strings.Contains(p, "/dists/"):
		return Mutable
	}
	return NoStore
}
