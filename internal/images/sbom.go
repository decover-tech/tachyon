package images

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Package is one component of an image.
type Package struct {
	Name    string
	Version string
	Arch    string // empty for non-Debian components
	// Kind is "deb" for dpkg packages, "toolcache" for runner tool cache
	// versions (/opt/hostedtoolcache/<name>/<version>).
	Kind string
}

// ParseDpkg reads `dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n'`.
func ParseDpkg(r io.Reader) ([]Package, error) {
	return parseTSV(r, "deb", 3)
}

// ParseToolcache reads "name<TAB>version" lines.
func ParseToolcache(r io.Reader) ([]Package, error) {
	return parseTSV(r, "toolcache", 2)
}

func parseTSV(r io.Reader, kind string, fields int) ([]Package, error) {
	var pkgs []Package
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != fields || f[0] == "" || f[1] == "" {
			return nil, fmt.Errorf("%s list line %d: want %d tab-separated fields: %q", kind, n, fields, line)
		}
		p := Package{Name: f[0], Version: f[1], Kind: kind}
		if fields == 3 {
			p.Arch = f[2]
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, sc.Err()
}

// purl is the package URL (https://github.com/package-url/purl-spec).
func (p Package) purl(distro string) string {
	switch p.Kind {
	case "deb":
		return fmt.Sprintf("pkg:deb/ubuntu/%s@%s?arch=%s&distro=%s",
			url.PathEscape(p.Name), url.PathEscape(p.Version), url.QueryEscape(p.Arch), url.QueryEscape(distro))
	default:
		return fmt.Sprintf("pkg:generic/%s@%s", url.PathEscape(p.Name), url.PathEscape(p.Version))
	}
}

type spdxDoc struct {
	SPDXVersion       string             `json:"spdxVersion"`
	DataLicense       string             `json:"dataLicense"`
	SPDXID            string             `json:"SPDXID"`
	Name              string             `json:"name"`
	DocumentNamespace string             `json:"documentNamespace"`
	CreationInfo      spdxCreation       `json:"creationInfo"`
	Packages          []spdxPackage      `json:"packages"`
	Relationships     []spdxRelationship `json:"relationships"`
}

type spdxCreation struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type spdxPackage struct {
	Name             string       `json:"name"`
	SPDXID           string       `json:"SPDXID"`
	VersionInfo      string       `json:"versionInfo"`
	DownloadLocation string       `json:"downloadLocation"`
	FilesAnalyzed    bool         `json:"filesAnalyzed"`
	ExternalRefs     []spdxExtRef `json:"externalRefs,omitempty"`
	PrimaryPurpose   string       `json:"primaryPackagePurpose,omitempty"`
}

type spdxExtRef struct {
	Category string `json:"referenceCategory"`
	Type     string `json:"referenceType"`
	Locator  string `json:"referenceLocator"`
}

type spdxRelationship struct {
	Element string `json:"spdxElementId"`
	Type    string `json:"relationshipType"`
	Related string `json:"relatedSpdxElement"`
}

// SBOM renders an SPDX 2.3 JSON document for an image. The output depends
// only on its inputs (packages are sorted, the timestamp is the build's
// SOURCE_DATE_EPOCH), so reproducible builds give identical SBOMs.
func SBOM(ref Ref, sourceDateEpoch int64, pkgs []Package) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	pkgs = append([]Package(nil), pkgs...)
	sort.Slice(pkgs, func(i, j int) bool {
		a, b := pkgs[i], pkgs[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Arch < b.Arch
	})

	// The namespace must be unique per document; derive it from the content.
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", ref)
	for _, p := range pkgs {
		fmt.Fprintf(h, "%s\t%s\t%s\t%s\n", p.Kind, p.Name, p.Version, p.Arch)
	}
	digest := hex.EncodeToString(h.Sum(nil))

	root := "SPDXRef-Image"
	doc := spdxDoc{
		SPDXVersion:       "SPDX-2.3",
		DataLicense:       "CC0-1.0",
		SPDXID:            "SPDXRef-DOCUMENT",
		Name:              ref.String(),
		DocumentNamespace: "https://github.com/decover-tech/tachyon/spdx/" + ref.Name + "/" + ref.Version + "/" + digest[:16],
		CreationInfo: spdxCreation{
			Created:  time.Unix(sourceDateEpoch, 0).UTC().Format(time.RFC3339),
			Creators: []string{"Tool: tachyon-image", "Organization: Tachyon"},
		},
		Packages: []spdxPackage{{
			Name:             ref.Name,
			SPDXID:           root,
			VersionInfo:      ref.Version,
			DownloadLocation: "NOASSERTION",
			PrimaryPurpose:   "OPERATING-SYSTEM",
		}},
		Relationships: []spdxRelationship{{Element: "SPDXRef-DOCUMENT", Type: "DESCRIBES", Related: root}},
	}
	distro := "ubuntu-" + strings.TrimPrefix(ref.Name, "ubuntu-")
	seen := map[string]int{}
	for _, p := range pkgs {
		id := spdxID(p)
		if n := seen[id]; n > 0 {
			id = fmt.Sprintf("%s-%d", id, n)
		}
		seen[spdxID(p)]++
		doc.Packages = append(doc.Packages, spdxPackage{
			Name:             p.Name,
			SPDXID:           id,
			VersionInfo:      p.Version,
			DownloadLocation: "NOASSERTION",
			ExternalRefs:     []spdxExtRef{{Category: "PACKAGE-MANAGER", Type: "purl", Locator: p.purl(distro)}},
		})
		doc.Relationships = append(doc.Relationships, spdxRelationship{Element: root, Type: "CONTAINS", Related: id})
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// spdxID makes an SPDX identifier: letters, digits, '.' and '-' only.
func spdxID(p Package) string {
	var b strings.Builder
	b.WriteString("SPDXRef-")
	b.WriteString(p.Kind)
	b.WriteByte('-')
	for _, r := range p.Name + "-" + p.Version + "-" + p.Arch {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.TrimRight(b.String(), "-")
}
