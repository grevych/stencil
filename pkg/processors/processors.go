// Copyright 2021 Outreach Corporation. All Rights Reserved.

// Description: Provides a processing framework. This is likely to be
// deprecated in favour of using the extension framework so use caution
// when writing a processor here.

// Package processors implements a file processing framework.
package processors

import (
	"io"
	"path/filepath"
	"reflect"

	"github.com/blang/semver/v4"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ErrNotProcessable means that a file didn't match a registered file name or
// extension by a processor to run. This really just means no-op occurred on
// the file in regards to it being processed.
var ErrNotProcessable = errors.New("not a processable file")

// registeredProcessors are the processors registered to run on each run of
// bootstrap.
var registeredProcessors = []Processor{}

// Config is the configuration metadata for a Processor.
type Config struct {
	// The name of the processor.
	Name string

	// FileExtensions are the file extensions this processor should handle
	FileExtensions []string

	// FileNames are specific files this processor should handle
	FileNames []string

	// IsPreCodegenProcessor denotes whether or not the given processor should
	// be ran before codegen starts.
	IsPreCodegenProcessor bool

	// IsPostCodegenProcessor denotes whether or not the given processor
	// should be ran after the codegen stage of bootstrap. The main difference
	// between running a processor during the codegen stage and after the
	// codegen stage are the files each touches, respectively. The codegen
	// processors will only touch non-static files generated by bootstrap,
	// whereas post-codegen processors will touch every file of the repository,
	// regardless of whether or not it was generated by bootstrap or not.
	// Template does not get passed to Process function (see Processor
	// interface) when this bool is set to true.
	IsPostCodegenProcessor bool

	// RerunCodegen denotes that after all of the processors have ran post-codegen,
	// rerun codegen. This will only fire if IsPostCodegenProcessor is also true.
	RerunCodegen bool

	// VersionGate denotes the version at which the processor should be ran and
	// implies that any versions before this version should also be ran on. For
	// example, if VersionGate was 8.1.4, the processor would be ran on all
	// previous versions of bootstrap (what is found in bootstrap.lock before
	// codegen begins) <= 8.1.4. It would not be ran on versions > 8.1.4.
	//
	// This also implies that if the previous version (what is found in bootstrap.lock
	// before codegen begins) is nil (which is the case when a repository is freshly
	// bootstrapped), the processor will not run as it shouldn't have any anything to
	// migrate on a fresh repository.
	VersionGate *semver.Version
}

// Processor defines the interface that a processor must implement to run.
type Processor interface {
	// Register configures this processor.
	Register() *Config

	// Config returns the config set on the processor, usually set by Register.
	Config() *Config

	// Process process a file and returns it
	Process(*File, *File) (*File, error)
}

// File is a small wrapper around an io.Reader, used for processing files
// in individual types that implement the Processor interface.
type File struct {
	io.Reader

	Name string
}

// Runner is used to execute several processors on individual files.
type Runner struct {
	processors []Processor

	// previousVersion denotes the version of bootstrap that the given
	// repository was previously on (found in their bootstrap.lock file
	// before codegen).
	previousVersion *semver.Version

	fileNames map[string][]Processor
	fileExts  map[string][]Processor
}

// NewFile create a new file. If r is nil, a nil file is returned.
func NewFile(r io.Reader, path string) *File {
	if r == nil || (reflect.ValueOf(r).Kind() == reflect.Ptr && reflect.ValueOf(r).IsNil()) {
		return nil
	}

	return &File{r, path}
}

// New creates a new runner using all of the registered processors in
// registeredProcecssors.
func New(log logrus.FieldLogger, previousVersion *semver.Version) *Runner {
	r := &Runner{
		processors:      registeredProcessors,
		previousVersion: previousVersion,
		fileNames:       make(map[string][]Processor),
		fileExts:        make(map[string][]Processor),
	}

	for i, p := range r.processors {
		cfg := p.Register()
		if cfg.VersionGate != nil && r.previousVersion != nil {
			if r.previousVersion.GT(*cfg.VersionGate) {
				// If the previous version is greater than the version gate, we skip
				// this processor.
				log.WithFields(logrus.Fields{
					"previousVersion": r.previousVersion.String(),
					"versionGate":     cfg.VersionGate.String(),
					"processor":       cfg.Name,
				}).Debug("skipping processor, found that previous version was greater than version gate.")
				continue
			}
		}

		// Register file extensions
		for _, str := range cfg.FileExtensions {
			if _, ok := r.fileExts[str]; !ok {
				r.fileExts[str] = make([]Processor, 0)
			}

			r.fileExts[str] = append(r.fileExts[str], r.processors[i])
		}

		// Register file names
		for _, str := range cfg.FileNames {
			if _, ok := r.fileNames[str]; !ok {
				r.fileNames[str] = make([]Processor, 0)
			}

			r.fileNames[str] = append(r.fileNames[str], r.processors[i])
		}
	}

	return r
}

// RunPreCodegen runs all of the pre-codegen processors.
func (r *Runner) RunPreCodegen(existing, template *File) (*File, error) {
	return r.process(true, false, existing, template)
}

// RunDuringCodegen runs all of the processors that are neither pre-codegen or post-codegen
// specific processors.
func (r *Runner) RunDuringCodegen(existing, template *File) (*File, error) {
	return r.process(false, false, existing, template)
}

// RunPostCodegen runs all of the post-codegen processors.
func (r *Runner) RunPostCodegen(existing, template *File) (*File, error) {
	return r.process(false, true, existing, template)
}

// process processes a file using all registered processors on the runner.
func (r *Runner) process(preCodegen, postCodegen bool, existing, template *File) (*File, error) {
	var ext, name string
	if preCodegen || postCodegen {
		ext = filepath.Ext(existing.Name)
		name = filepath.Base(existing.Name)
	} else {
		ext = filepath.Ext(template.Name)
		name = filepath.Base(template.Name)
	}

	var err error

	// touched denotes whether or not the given file was actually attempted to be processed
	// by any processor. It controls whether or not we return ErrNotProcessable.
	var touched bool

	// Capture the name in case Process call fails (rendering existing to nil)
	existingName := existing.Name

	for _, p := range r.fileExts[ext] {
		if preCodegen {
			if !p.Config().IsPreCodegenProcessor || p.Config().IsPostCodegenProcessor {
				// We're running pre-codegen, but the processor either isn't a pre-codegen
				// processor or is a post-codegen processor.
				continue
			}
		}

		if postCodegen {
			if !p.Config().IsPostCodegenProcessor || p.Config().IsPreCodegenProcessor {
				// We're running post-codegen, but the processor either isn't a post-codegen
				// processor or is a pre-codegen processor.
				continue
			}
		}

		touched = true

		// Overwrite exisiting with what is returned from the processor.
		if existing, err = p.Process(existing, template); err != nil {
			return nil, errors.Wrapf(err, "run %s processor on %s", p.Config().Name, existingName)
		}
	}

	for _, p := range r.fileNames[name] {
		if preCodegen {
			if !p.Config().IsPreCodegenProcessor || p.Config().IsPostCodegenProcessor {
				// We're running pre-codegen, but the processor either isn't a pre-codegen
				// processor or is a post-codegen processor.
				continue
			}
		}

		if postCodegen {
			if !p.Config().IsPostCodegenProcessor || p.Config().IsPreCodegenProcessor {
				// We're running post-codegen, but the processor either isn't a post-codegen
				// processor or is a pre-codegen processor.
				continue
			}
		}

		touched = true

		// Overwrite exisiting with what is returned from the processor.
		if existing, err = p.Process(existing, template); err != nil {
			return nil, errors.Wrapf(err, "run %s processor on %s", p.Config().Name, existingName)
		}
	}

	if !touched {
		return nil, ErrNotProcessable
	}
	return existing, err
}

// ShouldRerunPostCodegen checks to see if there are any post-codegen processors that require
// codegen to be reran after they run.
func (r *Runner) ShouldRerunPostCodegen() bool {
	for _, processors := range r.fileExts {
		for _, processor := range processors {
			if processor.Config().IsPostCodegenProcessor && processor.Config().RerunCodegen {
				return true
			}
		}
	}

	for _, processors := range r.fileNames {
		for _, processor := range processors {
			if processor.Config().IsPostCodegenProcessor && processor.Config().RerunCodegen {
				return true
			}
		}
	}

	return false
}
