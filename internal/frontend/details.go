// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package frontend

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"golang.org/x/discovery/internal"
	"golang.org/x/discovery/internal/derrors"
	"golang.org/x/discovery/internal/license"
	"golang.org/x/discovery/internal/log"
	"golang.org/x/discovery/internal/stdlib"
	"golang.org/x/discovery/internal/thirdparty/module"
	"golang.org/x/discovery/internal/thirdparty/semver"
	"golang.org/x/xerrors"
)

// DetailsPage contains data for a package of module details template.
type DetailsPage struct {
	basePage
	CanShowDetails bool
	Settings       TabSettings
	Details        interface{}
	Header         interface{}
	BreadcrumbPath template.HTML
	Tabs           []TabSettings
	Namespace      string
}

func (s *Server) handleDetails(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		s.staticPageHandler("index.tmpl", "go.dev")(w, r)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "@", 2)
	if stdlib.Contains(parts[0]) {
		s.handleStdLib(w, r)
		return
	}
	s.handlePackageDetails(w, r)
}

func (s *Server) handlePackageDetails(w http.ResponseWriter, r *http.Request) {
	pkgPath, modulePath, version, err := parseDetailsURLPath(r.URL.Path)
	if err != nil {
		log.Errorf("handlePackageDetails: %v", err)
		s.serveErrorPage(w, r, http.StatusBadRequest, nil)
		return
	}
	s.servePackagePage(w, r, pkgPath, modulePath, version)
}

// handlePackageDetailsRedirect redirects all redirects to "/pkg" to "/".
func (s *Server) handlePackageDetailsRedirect(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.TrimPrefix(r.URL.Path, "/pkg")
	http.Redirect(w, r, urlPath, http.StatusMovedPermanently)
}

// handleModuleDetails applies database data to the appropriate template.
// Handles all endpoints that match "/mod/<module-path>[@<version>?tab=<tab>]".
func (s *Server) handleModuleDetails(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.TrimPrefix(r.URL.Path, "/mod")
	path, _, version, err := parseDetailsURLPath(urlPath)
	if err != nil {
		log.Infof("handleModuleDetails: %v", err)
		s.serveErrorPage(w, r, http.StatusBadRequest, nil)
		return
	}
	s.serveModulePage(w, r, path, version)
}

// servePackagePage applies database data to the appropriate template.
// Handles all endpoints that match "/<import-path>[@<version>?tab=<tab>]".
func (s *Server) servePackagePage(w http.ResponseWriter, r *http.Request, pkgPath, modulePath, version string) {
	if version != internal.LatestVersion && !semver.IsValid(version) {
		epage := &errorPage{Message: fmt.Sprintf("%q is not a valid semantic version.", version)}
		epage.SecondaryMessage = suggestedSearch(pkgPath)
		s.serveErrorPage(w, r, http.StatusBadRequest, epage)
		return
	}

	var pkg *internal.VersionedPackage
	code, epage := fetchPackageOrModule(r.Context(), s.ds, "pkg", pkgPath, version, func(ver string) (string, error) {
		var err error
		pkg, err = s.ds.GetPackage(r.Context(), pkgPath, modulePath, ver)
		return modulePath, err
	})
	if code != http.StatusOK {
		if code == http.StatusNotFound {
			s.serveDirectoryPage(w, r, pkgPath, modulePath, version)
			return
		}
		s.serveErrorPage(w, r, code, epage)
		return
	}

	pkgHeader, err := createPackage(&pkg.Package, &pkg.VersionInfo, version == internal.LatestVersion)
	if err != nil {
		log.Errorf("error creating package header for %s@%s: %v", pkg.Path, pkg.Version, err)
		s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
		return
	}

	tab := r.FormValue("tab")
	settings, ok := packageTabLookup[tab]
	if !ok {
		var tab string
		if pkg.IsRedistributable() {
			tab = "doc"
		} else {
			tab = "overview"
		}
		http.Redirect(w, r, fmt.Sprintf(r.URL.Path+"?tab=%s", tab), http.StatusFound)
		return
	}
	canShowDetails := pkg.IsRedistributable() || settings.AlwaysShowDetails

	var details interface{}
	if canShowDetails {
		var err error
		details, err = fetchDetailsForPackage(r.Context(), r, tab, s.ds, pkg)
		if err != nil {
			log.Errorf("error fetching page for %q: %v", tab, err)
			s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
			return
		}
	}
	page := &DetailsPage{
		basePage:       newBasePage(r, packageTitle(&pkg.Package)),
		Settings:       settings,
		Header:         pkgHeader,
		BreadcrumbPath: breadcrumbPath(pkgHeader.Path, pkgHeader.Module.Path, pkgHeader.Module.Version),
		Details:        details,
		CanShowDetails: canShowDetails,
		Tabs:           packageTabSettings,
		Namespace:      "pkg",
	}
	s.servePage(w, settings.TemplateName, page)
}

// serveModulePage applies database data to the appropriate template.
func (s *Server) serveModulePage(w http.ResponseWriter, r *http.Request, modulePath, version string) {
	if version != internal.LatestVersion && !semver.IsValid(version) {
		epage := &errorPage{Message: fmt.Sprintf("%q is not a valid semantic version.", version)}
		s.serveErrorPage(w, r, http.StatusBadRequest, epage)
		return
	}

	ctx := r.Context()
	var moduleVersion *internal.VersionInfo
	code, epage := fetchPackageOrModule(ctx, s.ds, "mod", modulePath, version, func(ver string) (string, error) {
		var err error
		moduleVersion, err = s.ds.GetVersionInfo(ctx, modulePath, ver)
		return modulePath, err
	})
	if code != http.StatusOK {
		s.serveErrorPage(w, r, code, epage)
		return
	}

	// Here, moduleVersion is a valid *VersionInfo.
	licenses, err := s.ds.GetModuleLicenses(ctx, moduleVersion.ModulePath, moduleVersion.Version)
	if err != nil {
		log.Errorf("error getting module licenses for %s@%s: %v", moduleVersion.ModulePath, moduleVersion.Version, err)
		s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
		return
	}

	modHeader := createModule(moduleVersion, license.ToMetadatas(licenses), version == internal.LatestVersion)
	tab := r.FormValue("tab")
	settings, ok := moduleTabLookup[tab]
	if !ok {
		tab = "overview"
		settings = moduleTabLookup["overview"]
	}
	canShowDetails := modHeader.IsRedistributable || settings.AlwaysShowDetails
	var details interface{}
	if canShowDetails {
		var err error
		details, err = fetchDetailsForModule(ctx, r, tab, s.ds, moduleVersion, licenses)
		if err != nil {
			log.Errorf("error fetching page for %q: %v", tab, err)
			s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
			return
		}
	}
	page := &DetailsPage{
		basePage:       newBasePage(r, moduleTitle(moduleVersion.ModulePath)),
		Settings:       settings,
		Header:         modHeader,
		BreadcrumbPath: breadcrumbPath(moduleVersion.ModulePath, moduleVersion.ModulePath, moduleVersion.Version),
		Details:        details,
		CanShowDetails: canShowDetails,
		Tabs:           moduleTabSettings,
		Namespace:      "mod",
	}
	s.servePage(w, settings.TemplateName, page)
}

// fetchPackageOrModule handles logic common to the initial phase of
// handling both packages and modules: fetching information about the package
// or module.
//
// The get argument is a function that should retrieve a package or module at a
// given version. It returns the error from doing so, as well as the module
// path.
//
// fetchPackageOrModule parses urlPath into an import path and version, then
// calls the get function with those values. If get fails because the version
// cannot be found, fetchPackageOrModule calls get again with the latest
// version, to see if any versions of the package/module exist, in order to
// provide a more helpful error message.
//
// fetchPackageOrModule returns the import path and version requested, an
// HTTP status code, and possibly an error page to display.
func fetchPackageOrModule(ctx context.Context, ds DataSource, namespace, path, version string, get func(v string) (string, error)) (code int, _ *errorPage) {
	excluded, err := ds.IsExcluded(ctx, path)
	if err != nil {
		log.Errorf("error checking excluded path: %v", err)
		return http.StatusInternalServerError, nil
	}
	if excluded {
		// Return NotFound; don't let the user know that the package was excluded.
		return http.StatusNotFound, nil
	}

	// Fetch the package or module from the database.
	_, err = get(version)
	if err == nil {
		// A package or module was found for this path and version.
		return http.StatusOK, nil
	}
	if !xerrors.Is(err, derrors.NotFound) {
		// Something went wrong in executing the get function.
		log.Errorf("fetchPackageOrModule %s@%s: %v", path, version, err)
		return http.StatusInternalServerError, nil
	}
	if version == internal.LatestVersion {
		// We were not able to find a module or package at any version.
		return http.StatusNotFound, nil
	}

	// We did not find the given version, but maybe there is another version
	// available for this package or module.
	modulePath, err := get(internal.LatestVersion)
	if err != nil {
		if !xerrors.Is(err, derrors.NotFound) {
			log.Errorf("error: get(%s, Latest) for %s: %v", path, namespace, err)
		}
		// Couldn't get the latest version, for whatever reason. Treat
		// this like not finding the original version.
		return http.StatusNotFound, nil
	}

	// There is a later version of this package/module.
	word := "package"
	urlPath := "/" + path
	if namespace == "mod" {
		word = "module"
		urlPath = "/mod/" + path
	}
	epage := &errorPage{
		Message: fmt.Sprintf("%s %s@%s is not available.",
			strings.Title(word), path, formattedVersion(version, modulePath)),
		SecondaryMessage: template.HTML(
			fmt.Sprintf(`There are other versions of this %s that are! To view them, <a href="%s?tab=versions">click here</a>.</p>`, word, urlPath)),
	}
	return http.StatusSeeOther, epage
}

// parseDetailsURLPath returns the modulePath (if known),
// pkgPath and version specified by urlPath.
// urlPath is assumed to be a valid path following the structure:
//   /<module-path>[@<version>/<suffix>]
//
// If <version> is not specified, internal.LatestVersion is used for the
// version. modulePath can only be determined if <version> is specified.
//
// Leading and trailing slashes in the urlPath are trimmed.
func parseDetailsURLPath(urlPath string) (pkgPath, modulePath, version string, err error) {
	defer derrors.Wrap(&err, "parseDetailsURLPath(%q)", urlPath)

	// This splits urlPath into either:
	//   /<module-path>[/<suffix>]
	// or
	//   /<module-path>, @<version>/<suffix>
	// or
	//  /<module-path>/<suffix>, @<version>
	// TODO(b/140191811) The last URL route should redirect.
	parts := strings.SplitN(urlPath, "@", 2)
	basePath := strings.TrimSuffix(strings.TrimPrefix(parts[0], "/"), "/")
	if len(parts) == 1 {
		modulePath = internal.UnknownModulePath
		version = internal.LatestVersion
		pkgPath = basePath
	} else {
		// Parse the version and suffix from parts[1].
		endParts := strings.Split(parts[1], "/")
		suffix := strings.Join(endParts[1:], "/")
		version = endParts[0]
		if suffix == "" || version == internal.LatestVersion {
			modulePath = internal.UnknownModulePath
			pkgPath = basePath
		} else {
			modulePath = basePath
			pkgPath = basePath + "/" + suffix
		}
	}
	if err := module.CheckImportPath(pkgPath); err != nil {
		return "", "", "", fmt.Errorf("malformed path %q: %v", pkgPath, err)
	}
	if stdlib.Contains(pkgPath) {
		modulePath = stdlib.ModulePath
	}
	return pkgPath, modulePath, version, nil
}

// handleLatestVersion writes a JSON string with the latest version of the package or module.
// It expects URLs of the form
//   /latest-version/MODULE_PATH
// for modules, or
//   /latest-version/MODULE_PATH?pkg=PACKAGE_PATH
// for packages.
func (s *Server) handleLatestVersion(w http.ResponseWriter, r *http.Request) {
	modulePath := strings.TrimPrefix(r.URL.Path, "/latest-version/")
	packagePath := r.URL.Query().Get("pkg")
	v := s.LatestVersion(r.Context(), modulePath, packagePath)
	if _, err := fmt.Fprintf(w, "%q", v); err != nil {
		log.Errorf("handleLatestVersion: fmt.Fprintf: %v", err)
	}
}

// LatestVersion returns the latest version of the package or module.
// It returns the empty string on error.
// It is intended to be used as an argument to middleware.LatestVersion.
func (s *Server) LatestVersion(ctx context.Context, modulePath, packagePath string) string {
	v, err := s.latestVersion(ctx, modulePath, packagePath)
	if err != nil {
		// We get NotFound errors from directories; they clutter the log.
		if !xerrors.Is(err, derrors.NotFound) {
			log.Errorf("GetLatestVersion: %v", err)
		}
		return ""
	}
	return v
}

func (s *Server) latestVersion(ctx context.Context, modulePath, packagePath string) (_ string, err error) {
	defer derrors.Wrap(&err, "latestVersion(ctx, %q, %q)", modulePath, packagePath)

	var vi *internal.VersionInfo
	if packagePath == "" {
		vi, err = s.ds.GetVersionInfo(ctx, modulePath, internal.LatestVersion)
		if err != nil {
			return "", err
		}
	} else {
		pkg, err := s.ds.GetPackage(ctx, packagePath, modulePath, internal.LatestVersion)
		if err != nil {
			return "", err
		}
		vi = &pkg.VersionInfo
	}
	v := vi.Version
	if modulePath == stdlib.ModulePath {
		v, err = stdlib.TagForVersion(v)
		if err != nil {
			return "", err
		}
	}
	return v, nil
}
