// Copyright 2022 Outreach Corporation. All Rights Reserved.

// Description: See package description.

// Package extensions consumes extensions in stencil
package extensions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/getoutreach/gobox/pkg/cli/github"
	"github.com/getoutreach/gobox/pkg/updater"
	"github.com/getoutreach/stencil/pkg/extensions/apiv1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	giturls "github.com/whilp/git-urls"
)

// generatedTemplateFunc is the underlying type of a function
// generated by createFunctionFromTemplateFunction that's used
// to wrap the go plugin call to invoke said function
type generatedTemplateFunc func(...interface{}) (interface{}, error)

// Host implements an extension host that handles
// registering extensions and executing them.
type Host struct {
	log        logrus.FieldLogger
	extensions map[string]apiv1.Implementation
}

// NewHost creates a new extension host
func NewHost(log logrus.FieldLogger) *Host {
	return &Host{
		log:        log,
		extensions: make(map[string]apiv1.Implementation),
	}
}

// createFunctionFromTemplateFunction takes a given
// TemplateFunction and turns it into a callable function
func (h *Host) createFunctionFromTemplateFunction(extName string, ext apiv1.Implementation,
	fn *apiv1.TemplateFunction) generatedTemplateFunc {
	extPath := extName + "." + fn.Name

	return func(args ...interface{}) (interface{}, error) {
		if len(args) > fn.NumberOfArguments {
			return nil, fmt.Errorf("too many arguments, expected %d, got %d", fn.NumberOfArguments, len(args))
		}

		resp, err := ext.ExecuteTemplateFunction(&apiv1.TemplateFunctionExec{
			Name:      fn.Name,
			Arguments: args,
		})
		if err != nil {
			// return an error if the extension returns an error
			return nil, errors.Wrapf(err, "failed to execute template function %q", extPath)
		}

		// return the response, and a nil error
		return resp, nil
	}
}

// GetExtensionCaller returns an extension caller that's
// aware of all extension functions
func (h *Host) GetExtensionCaller(ctx context.Context) (*ExtensionCaller, error) {
	// funcMap stores the extension functions discovered
	funcMap := map[string]map[string]generatedTemplateFunc{}

	// Call all extensions to get the template functions provided
	for extName, ext := range h.extensions {
		funcs, err := ext.GetTemplateFunctions()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get template functions from plugin '%s'", extName)
		}

		for _, f := range funcs {
			h.log.WithField("extension", extName).WithField("function", f.Name).Debug("Registering extension function")
			tfunc := h.createFunctionFromTemplateFunction(extName, ext, f)

			if _, ok := funcMap[extName]; !ok {
				funcMap[extName] = make(map[string]generatedTemplateFunc)
			}
			funcMap[extName][f.Name] = tfunc
		}
	}

	// return the lookup function, used via Call()
	return &ExtensionCaller{funcMap}, nil
}

// RegisterExtension registers a ext from a given source
// and compiles/downloads it. A client is then created
// that is able to communicate with the ext.
func (h *Host) RegisterExtension(ctx context.Context, source, name string) error { //nolint:funlen // Why: OK length.
	h.log.WithField("extension", name).WithField("source", source).Debug("Registered extension")

	u, err := giturls.Parse(source)
	if err != nil {
		return errors.Wrap(err, "failed to parse extension URL")
	}

	var extPath string
	if u.Scheme == "file" {
		extPath = filepath.Join(strings.TrimPrefix(source, "file://"), "bin", "plugin")
	} else {
		pathSpl := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(pathSpl) < 2 {
			return fmt.Errorf("invalid repository, expected org/repo, got %s", u.Path)
		}
		extPath, err = h.downloadFromRemote(ctx, pathSpl[0], pathSpl[1], name)
	}
	if err != nil {
		return errors.Wrap(err, "failed to setup extension")
	}

	ext, err := apiv1.NewExtensionClient(ctx, extPath, h.log)
	if err != nil {
		return err
	}

	if _, err := ext.GetConfig(); err != nil {
		return errors.Wrap(err, "failed to get config from extension")
	}
	h.extensions[name] = ext

	return nil
}

// getExtensionPath returns the path to an extension binary
func (h *Host) getExtensionPath(version, name, repo string) string {
	homeDir, _ := os.UserHomeDir() //nolint:errcheck // Why: signature doesn't allow it, yet
	path := filepath.Join(homeDir, ".outreach", ".config", "stencil", "extensions", name, fmt.Sprintf("@%s", version), repo)
	os.MkdirAll(filepath.Dir(path), 0o755) //nolint:errcheck // Why: signature doesn't allow it, yet
	return path
}

// downloadFromRemote downloads a release from github and extracts it to disk
//
// using the example extension module: github.com/getoutreach/stencil-plugin
// 	org: getoutreach
// 	repo: stencil-plugin
// 	name: github.com/getoutreach/stencil-plugin
func (h *Host) downloadFromRemote(ctx context.Context, org, repo, name string) (string, error) {
	ghc, err := github.NewClient(github.WithAllowUnauthenticated(), github.WithLogger(h.log))
	if err != nil {
		return "", err
	}

	gh := updater.NewGithubUpdaterWithClient(ctx, ghc, org, repo)
	err = gh.Check(ctx)
	if err != nil {
		return "", errors.Wrap(err, "failed to validate github client worked")
	}

	rel, err := gh.GetLatestVersion(ctx, "v0.0.0", false)
	if err != nil {
		return "", errors.Wrap(err, "failed to find latest extension version")
	}

	// Check if the version we're pulling already exists and is exectuable before downloading
	// it again.
	dlPath := h.getExtensionPath(rel.GetTagName(), name, repo)
	if info, err := os.Stat(dlPath); err == nil && info.Mode() == 0o755 {
		return dlPath, nil
	}

	// Binary for plugin at version we want doesn't exist on disk, need to download.
	bin, cleanup, err := gh.DownloadRelease(ctx, rel, repo, repo)
	if err != nil {
		return "", errors.Wrap(err, "failed to download extension")
	}
	defer cleanup()

	// Move the downloaded release from where the updater put it to where we need it
	// for stencil.
	if err := os.Rename(bin, dlPath); err != nil {
		return "", errors.Wrap(err, "failed to move downloaded extension")
	}

	// Ensure the file is executable.
	if err := os.Chmod(dlPath, 0o755); err != nil {
		return "", errors.Wrap(err, "ensure plugin is executable")
	}

	return dlPath, nil
}
