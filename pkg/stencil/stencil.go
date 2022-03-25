// Copyright 2022 Outreach Corporation. All Rights Reserved.

// Description: API for interacting with stencil.

// Package stencil provides an entry point for interacting with Stencil.
package stencil

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// This block contains constants of the lockfiles
const (
	// LockfileName is the name of the lockfile used by stencil
	LockfileName = "stencil.lock"

	// oldLockfileName is the old lockfile that stencil interops with
	oldLockfileName = "bootstrap.lock"
)

// LockfileModuleEntry is an entry in the lockfile for a module
// that was used during the last run of stencil.
type LockfileModuleEntry struct {
	// Name is the name of the module. This usually comes from
	// the TemplateManifest entry, but is up to the module
	// package.
	Name string

	// URL is the url of the module that was used.
	URL string

	// Version is the version of the module that was
	// downloaded at the time.
	Version string
}

// LockfileFileEntry is an entry in the lockfile for a file
// that was generated by stencil. This contains metadata on what
// generated it among other future information
type LockfileFileEntry struct {
	// Name is the relative file path, to the invocation of stencil,
	// of the generated file
	Name string

	// Template is the template that generated this file in the given
	// module.
	Template string

	// Module is the URL of the module that generated this file.
	Module string
}

// Lockfile is generated by stencil on a ran to store version
// information.
type Lockfile struct {
	// Version correlates to the version of bootstrap
	// that generated this file.
	Version string `yaml:"version"`

	// Generated was the last time this file was modified
	Generated time.Time `yaml:"generated"`

	// Modules is a list of modules and their versions that was
	// used the last time stencil was ran.
	// Note: This is only set in stencil.lock
	Modules []*LockfileModuleEntry `yaml:"modules"`

	// Files is a list of files and metadata about them that were
	// generated by stencil
	Files []*LockfileFileEntry `yaml:"files"`
}

// LoadLockfile loads a lockfile from a bootstrap
// repository path
func LoadLockfile(path string) (*Lockfile, error) {
	f, err := os.Open(filepath.Join(path, LockfileName))
	if errors.Is(err, os.ErrNotExist) {
		f, err = os.Open(oldLockfileName)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	defer f.Close()

	var lock *Lockfile
	err = yaml.NewDecoder(f).Decode(&lock)
	return lock, err
}
