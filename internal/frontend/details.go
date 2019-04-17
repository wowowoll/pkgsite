// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package frontend

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/russross/blackfriday/v2"
	"golang.org/x/discovery/internal"
	"golang.org/x/discovery/internal/postgres"
	"golang.org/x/discovery/internal/thirdparty/module"
	"golang.org/x/discovery/internal/thirdparty/semver"
)

// DetailsPage contains data for the doc template.
type DetailsPage struct {
	Details       interface{}
	PackageHeader *Package
}

// OverviewDetails contains all of the data that the overview template
// needs to populate.
type OverviewDetails struct {
	ModulePath string
	ReadMe     template.HTML
}

// DocumentationDetails contains data for the doc template.
type DocumentationDetails struct {
	ModulePath string
}

// ModuleDetails contains all of the data that the module template
// needs to populate.
type ModuleDetails struct {
	ModulePath string
	Version    string
	ReadMe     template.HTML
	Packages   []*Package
}

// VersionsDetails contains all the data that the versions tab
// template needs to populate.
type VersionsDetails struct {
	Versions []*MajorVersionGroup
}

// ImportsDetails contains information for a package's imports.
type ImportsDetails struct {
}

// ImportersDetails contains information for importers of a package.
type ImportersDetails struct {
}

// LicensesDetails contains license information for a package.
type LicensesDetails struct {
}

// Package contains information for an individual package.
type Package struct {
	Version    string
	Path       string
	ModulePath string
	Synopsis   string
	CommitTime string
	Name       string
	Licenses   []*internal.LicenseInfo
	dbname     string
}

// Title returns p.Name(), prefixed by "Command" if p.IsCommand() is true and
// "Package" if it is not.
func (p *Package) Title() string {
	if p.IsCommand() {
		return fmt.Sprintf("Command %s", p.Name())
	}
	return fmt.Sprintf("Package %s", p.Name())
}

// Name returns dname if dbname is not "main". Otherwise, it returns the last
// element of dbname that is not a major version identifier (such as "v2"),
// prefixed by Command. For example, if p.dbname is "main" and p.Path is
// foo/bar/v2, it will return Command bar.
func (p *Package) Name() string {
	if p.dbname != "main" {
		return p.dbname
	}

	if p.Path[len(p.Path)-3:] == "/v1" {
		return filepath.Base(p.Path[:len(p.Path)-3])
	}
	path, _, _ := module.SplitPathVersion(p.Path)
	return filepath.Base(path)
}

// IsCommand reports whether p is a Go command, based on whether p.dbname is
// "main".
func (p *Package) IsCommand() bool {
	return p.dbname == "main"
}

// Dir returns the directory of the package relative to the root of the module.
func (p *Package) Dir() string {
	return strings.TrimPrefix(p.Path, fmt.Sprintf("%s/", p.ModulePath))
}

// MajorVersionGroup represents the major level of the versions
// list hierarchy (i.e. "v1").
type MajorVersionGroup struct {
	Level    string
	Latest   *Package
	Versions []*MinorVersionGroup
}

// MinorVersionGroup represents the major/minor level of the versions
// list hierarchy (i.e. "1.5").
type MinorVersionGroup struct {
	Level    string
	Latest   *Package
	Versions []*Package
}

// elapsedTime takes a date and returns returns human-readable,
// relative timestamps based on the following rules:
// (1) 'X hours ago' when X < 6
// (2) 'today' between 6 hours and 1 day ago
// (3) 'Y days ago' when Y < 6
// (4) A date formatted like "Jan 2, 2006" for anything further back
func elapsedTime(date time.Time) string {
	elapsedHours := int(time.Since(date).Hours())
	if elapsedHours == 1 {
		return "1 hour ago"
	} else if elapsedHours < 6 {
		return fmt.Sprintf("%d hours ago", elapsedHours)
	}

	elapsedDays := elapsedHours / 24
	if elapsedDays < 1 {
		return "today"
	} else if elapsedDays == 1 {
		return "1 day ago"
	} else if elapsedDays < 6 {
		return fmt.Sprintf("%d days ago", elapsedDays)
	}

	return date.Format("Jan _2, 2006")
}

func fetchPackageHeader(ctx context.Context, db *postgres.DB, path, version string) (*Package, error) {
	pkg, err := db.GetPackage(ctx, path, version)
	if err != nil {
		return nil, fmt.Errorf("db.GetPackage(ctx, %q, %q): %v", path, version, err)
	}

	pkgHeader, err := createPackageHeader(pkg)
	if err != nil {
		return nil, fmt.Errorf("createPackageHeader(%+v): %v", pkg, err)
	}
	return pkgHeader, nil
}

// createPackageHeader returns a *Package based on the fields
// of the specified package. It assumes that pkg is not nil.
func createPackageHeader(pkg *internal.Package) (*Package, error) {
	if pkg == nil {
		return nil, fmt.Errorf("package cannot be nil")
	}
	if pkg.Version == nil {
		return nil, fmt.Errorf("package's version cannot be nil")
	}

	return &Package{
		Name:       pkg.Name,
		Version:    pkg.Version.Version,
		Path:       pkg.Path,
		Synopsis:   pkg.Synopsis,
		Licenses:   pkg.Licenses,
		CommitTime: elapsedTime(pkg.Version.CommitTime),
	}, nil
}

// fetchOverviewDetails fetches data for the module version specified by path and version
// from the database and returns a OverviewDetails.
func fetchOverviewDetails(ctx context.Context, db *postgres.DB, path, version string) (*DetailsPage, error) {
	pkg, err := db.GetPackage(ctx, path, version)
	if err != nil {
		return nil, fmt.Errorf("db.GetPackage(ctx, %q, %q): %v", path, version, err)
	}

	pkgHeader, err := createPackageHeader(pkg)
	if err != nil {
		return nil, fmt.Errorf("createPackageHeader(%+v): %v", pkg, err)
	}
	return &DetailsPage{
		PackageHeader: pkgHeader,
		Details: &OverviewDetails{
			ModulePath: pkg.Version.Module.Path,
			ReadMe:     readmeHTML(pkg.Version.ReadMe),
		},
	}, nil
}

// fetchDocumentationDetails fetches data for the package specified by path and version
// from the database and returns a DocumentationDetails.
func fetchDocumentationDetails(ctx context.Context, db *postgres.DB, path, version string) (*DetailsPage, error) {
	pkgHeader, err := fetchPackageHeader(ctx, db, path, version)
	if err != nil {
		return nil, fmt.Errorf("db.fetchPackageHeader(ctx, db, %q, %q): %v", path, version, err)
	}
	return &DetailsPage{
		PackageHeader: pkgHeader,
		Details: &DocumentationDetails{
			ModulePath: pkgHeader.ModulePath,
		},
	}, nil
}

// fetchModuleDetails fetches data for the module version specified by pkgPath and pkgversion
// from the database and returns a ModuleDetails.
func fetchModuleDetails(ctx context.Context, db *postgres.DB, pkgPath, pkgversion string) (*DetailsPage, error) {
	version, err := db.GetVersionForPackage(ctx, pkgPath, pkgversion)
	if err != nil {
		return nil, fmt.Errorf("db.GetVersionForPackage(ctx, %q, %q): %v", pkgPath, pkgversion, err)
	}

	var (
		pkgHeader *Package
		packages  []*Package
	)
	for _, p := range version.Packages {
		packages = append(packages, &Package{
			Name:       p.Name,
			Path:       p.Path,
			Synopsis:   p.Synopsis,
			Licenses:   p.Licenses,
			Version:    version.Version,
			ModulePath: version.Module.Path,
		})

		if p.Path == pkgPath {
			p.Version = &internal.Version{
				Version:    version.Version,
				CommitTime: version.CommitTime,
			}
			pkgHeader, err = createPackageHeader(p)
			if err != nil {
				return nil, fmt.Errorf("createPackageHeader(%+v): %v", p, err)
			}
		}
	}

	return &DetailsPage{
		PackageHeader: pkgHeader,
		Details: &ModuleDetails{
			ModulePath: version.Module.Path,
			Version:    pkgversion,
			ReadMe:     readmeHTML(version.ReadMe),
			Packages:   packages,
		},
	}, nil
}

// fetchVersionsDetails fetches data for the module version specified by path and version
// from the database and returns a VersionsDetails.
func fetchVersionsDetails(ctx context.Context, db *postgres.DB, path, version string) (*DetailsPage, error) {
	versions, err := db.GetTaggedVersionsForPackageSeries(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("db.GetTaggedVersions(%q): %v", path, err)
	}

	// If no tagged versions for the package series are found,
	// fetch the pseudo-versions instead.
	if len(versions) == 0 {
		versions, err = db.GetPseudoVersionsForPackageSeries(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("db.GetPseudoVersions(%q): %v", path, err)
		}
	}

	var (
		pkgHeader       = &Package{}
		mvg             = []*MajorVersionGroup{}
		prevMajor       = ""
		prevMajMin      = ""
		prevMajorIndex  = -1
		prevMajMinIndex = -1
	)

	for _, v := range versions {
		vStr := v.Version
		if vStr == version {
			pkg := &internal.Package{
				Path:     path,
				Name:     v.Packages[0].Name,
				Synopsis: v.Synopsis,
				Licenses: v.Packages[0].Licenses,
				Version: &internal.Version{
					Version:    version,
					CommitTime: v.CommitTime,
				},
			}
			pkgHeader, err = createPackageHeader(pkg)
			if err != nil {
				return nil, fmt.Errorf("createPackageHeader(%+v): %v", pkg, err)
			}
		}

		major := semver.Major(vStr)
		majMin := strings.TrimPrefix(semver.MajorMinor(vStr), "v")
		fullVersion := strings.TrimPrefix(vStr, "v")

		if prevMajor != major {
			prevMajorIndex = len(mvg)
			prevMajor = major
			mvg = append(mvg, &MajorVersionGroup{
				Level: major,
				Latest: &Package{
					Version:    fullVersion,
					Path:       v.Packages[0].Path,
					CommitTime: elapsedTime(v.CommitTime),
				},
				Versions: []*MinorVersionGroup{},
			})
		}

		if prevMajMin != majMin {
			prevMajMinIndex = len(mvg[prevMajorIndex].Versions)
			prevMajMin = majMin
			mvg[prevMajorIndex].Versions = append(mvg[prevMajorIndex].Versions, &MinorVersionGroup{
				Level: majMin,
				Latest: &Package{
					Version:    fullVersion,
					Path:       v.Packages[0].Path,
					CommitTime: elapsedTime(v.CommitTime),
				},
			})
		}

		mvg[prevMajorIndex].Versions[prevMajMinIndex].Versions = append(mvg[prevMajorIndex].Versions[prevMajMinIndex].Versions, &Package{
			Version:    fullVersion,
			Path:       v.Packages[0].Path,
			CommitTime: elapsedTime(v.CommitTime),
		})
	}

	return &DetailsPage{
		PackageHeader: pkgHeader,
		Details: &VersionsDetails{
			Versions: mvg,
		},
	}, nil
}

// fetchLicensesDetails fetches license data for the package version specified by
// path and version from the database and returns a LicensesDetails.
func fetchLicensesDetails(ctx context.Context, db *postgres.DB, path, version string) (*DetailsPage, error) {
	pkgHeader, err := fetchPackageHeader(ctx, db, path, version)
	if err != nil {
		return nil, fmt.Errorf("db.fetchPackageHeader(ctx, db, %q, %q): %v", path, version, err)
	}
	return &DetailsPage{
		PackageHeader: pkgHeader,
	}, nil
}

// fetchImportsDetails fetches imports for the package version specified by
// path and version from the database and returns a ImportsDetails.
func fetchImportsDetails(ctx context.Context, db *postgres.DB, path, version string) (*DetailsPage, error) {
	pkgHeader, err := fetchPackageHeader(ctx, db, path, version)
	if err != nil {
		return nil, fmt.Errorf("db.fetchPackageHeader(ctx, db, %q, %q): %v", path, version, err)
	}
	return &DetailsPage{
		PackageHeader: pkgHeader,
	}, nil
}

// fetchImportersDetails fetches importers for the package version specified by
// path and version from the database and returns a ImportersDetails.
func fetchImportersDetails(ctx context.Context, db *postgres.DB, path, version string) (*DetailsPage, error) {
	pkgHeader, err := fetchPackageHeader(ctx, db, path, version)
	if err != nil {
		return nil, fmt.Errorf("db.fetchPackageHeader(ctx, db, %q, %q): %v", path, version, err)
	}
	return &DetailsPage{
		PackageHeader: pkgHeader,
	}, nil
}

func readmeHTML(readme []byte) template.HTML {
	unsafe := blackfriday.Run(readme)
	b := bluemonday.UGCPolicy().SanitizeBytes(unsafe)
	return template.HTML(string(b))
}

// HandleDetails applies database data to the appropriate template. Handles all
// endpoints that match "/" or "/<import-path>[@<version>?tab=<tab>]"
func (c *Controller) HandleDetails(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		c.renderPage(w, "index.tmpl", nil)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	if err := module.CheckPath(path); err != nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		log.Printf("Malformed path %q: %v", path, err)
		return
	}
	version := r.FormValue("v")
	if version != "" && !semver.IsValid(version) {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		log.Printf("Malformed version %q", version)
		return
	}

	var (
		page *DetailsPage
		err  error
		ctx  = r.Context()
	)

	tab := r.FormValue("tab")
	switch tab {
	case "doc":
		page, err = fetchDocumentationDetails(ctx, c.db, path, version)
	case "versions":
		page, err = fetchVersionsDetails(ctx, c.db, path, version)
	case "module":
		page, err = fetchModuleDetails(ctx, c.db, path, version)
	case "imports":
		page, err = fetchImportsDetails(ctx, c.db, path, version)
	case "importers":
		page, err = fetchImportersDetails(ctx, c.db, path, version)
	case "licenses":
		page, err = fetchLicensesDetails(ctx, c.db, path, version)
	case "overview":
		fallthrough
	default:
		tab = "overview"
		page, err = fetchOverviewDetails(ctx, c.db, path, version)
	}

	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		log.Printf("error fetching page for %q: %v", tab, err)
		return
	}

	c.renderPage(w, tab+".tmpl", page)
}
