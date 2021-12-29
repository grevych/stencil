// Copyright 2021 Outreach Corporation. All Rights Reserved.

// Description: See package description

// Package codegen has code generators for Go projects
//
// This is intended for use with stencil but can also be used
// outside of it.
//
// Using configuration.ServiceManifest, a list of template repositories
// is created and cloned into a layered filesystem with the sub-dependencies
// of the root dependency (the module) being used first, and so on. This layered
// fs is then walked to find all files with a `.tpl` extension. These are rendred
// and turned into functions.RenderedTemplate objects, and then written to disk
// based on the template's function calls.
//
// This is the core of stencil
package codegen

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/blang/semver/v4"
	"github.com/getoutreach/gobox/pkg/app"
	"github.com/getoutreach/gobox/pkg/box"
	"github.com/getoutreach/gobox/pkg/cfg"
	"github.com/getoutreach/stencil/internal/vfs"
	"github.com/getoutreach/stencil/pkg/configuration"
	"github.com/getoutreach/stencil/pkg/extensions"
	"github.com/getoutreach/stencil/pkg/functions"
	"github.com/getoutreach/stencil/pkg/processors"
	"github.com/getoutreach/stencil/pkg/stencil"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

var (
	ErrNotAFile           = errors.New("not a file")
	ErrNoHeadBranch       = errors.New("failed to find a head branch, does one exist?")
	ErrNoRemoteHeadBranch = errors.New("failed to get head branch from remote origin")

	blockPattern = regexp.MustCompile(`\w*(///|###|<!---)\s*([a-zA-Z ]+)\(([a-zA-Z ]+)\)`)
	headPattern  = regexp.MustCompile(`HEAD branch: ([[:alpha:]]+)`)

	// versionPattern ensures versions have at least a major and a minor.
	//
	// For examples, see https://regex101.com/r/ajHtpK/1
	versionPattern = regexp.MustCompile(`^\d+\.\d+[.\d+]*$`)
)

type Builder struct {
	Branch    string
	Repo      string
	Dir       string
	Manifest  *configuration.ServiceManifest
	GitRepoFs billy.Filesystem

	Processors      *processors.Runner
	extensions      *extensions.Host
	extensionCaller *extensions.ExtensionCaller

	log logrus.FieldLogger

	sshKeyPath  string
	accessToken cfg.SecretData

	// set by Run
	postRunCommands []*configuration.PostRunCommandSpec
}

// NewBuilder returns a new builder
func NewBuilder(repo, dir string, log logrus.FieldLogger, s *configuration.ServiceManifest,
	sshKeyPath string, accessToken cfg.SecretData) *Builder {
	// previousVersion is the previous version of bootstrap last run on this repository.
	// This will be passed to the builder as nil if this is a fresh repository.
	var previousVersion *semver.Version

	lock, err := stencil.LoadLockfile("")
	//nolint:gocritic // Why: case doesn't support errors.Is
	if err == nil {
		version, err := semver.ParseTolerant(lock.Version)
		if err == nil {
			previousVersion = &version
			log.WithField("previousVersion", previousVersion.String()).Info("found previous version of bootstrap")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		// noop
	} else {
		log.WithError(err).Warn("failed to load lockfile")
	}

	return &Builder{
		Repo:       repo,
		Dir:        dir,
		Manifest:   s,
		Processors: processors.New(logrus.New(), previousVersion),
		extensions: extensions.NewHost(),

		sshKeyPath:  sshKeyPath,
		accessToken: accessToken,

		postRunCommands: make([]*configuration.PostRunCommandSpec, 0),
	}
}

// Run fetches dependencies of the root modules and builds the layered filesystem,
// after that GenerateFiles is called to actually walk the filesystem and render
// the templates. This step also does minimal post-processing of the dependencies
// manifes.yamls.
func (b *Builder) Run(ctx context.Context) ([]string, error) {
	if err := b.processManifest(); err != nil {
		return nil, errors.Wrap(err, "failed to process service manifest")
	}

	b.log.Info("Fetching dependencies")
	fetcher := NewFetcher(b.log, b.Manifest, b.sshKeyPath, b.accessToken, b.extensions)
	fs, manifests, err := fetcher.CreateVFS(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create vfs")
	}
	b.GitRepoFs = fs

	ec, err := b.extensions.GetExtensionCaller(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get template functions from extensions")
	}
	b.extensionCaller = ec

	for _, m := range manifests {
		b.postRunCommands = append(b.postRunCommands, m.PostRunCommand...)
	}

	return b.GenerateFiles(ctx, fs)
}

// processManifest handles processing any fields in the manifest, i.e validation
func (b *Builder) processManifest() error {
	for resource, version := range b.Manifest.Versions {
		if !versionPattern.MatchString(version) {
			return fmt.Errorf("resource \"%s\" must have at least a major and minor version (format: MAJOR.MINOR.PATCH)", resource)
		}
	}

	return nil
}

func (b *Builder) FormatFiles(ctx context.Context) error {
	b.log.Info("Running post-run commands")
	for _, prc := range b.postRunCommands {
		b.log.Infof(" - %s", prc.Name)
		cmd := exec.CommandContext(ctx, "env", "bash", "-c", prc.Command) //nolint:gosec // Why: We have to
		cmd.Dir = b.Dir
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return errors.Wrapf(err, "failed to run '%s'", prc.Command)
		}
	}

	return nil
}

// GenerateFiles walks the vfs generated by Run() and renders the templates
func (b *Builder) GenerateFiles(ctx context.Context, fs billy.Filesystem) ([]string, error) {
	data, err := b.makeTemplateParameters(ctx)
	if err != nil {
		return nil, err
	}

	warnings := make([]string, 0)
	return warnings, vfs.Walk(fs, "", func(path string, file os.FileInfo, err error) error {
		if err != nil {
			return errors.Wrapf(err, "failed to read %s", path)
		}

		// Skip files without a .tpl extension
		if filepath.Ext(path) != ".tpl" {
			return nil
		}

		contents, err := b.FetchTemplate(ctx, path)
		if err != nil {
			return errors.Wrap(err, "failed to fetch template")
		}

		byt, err := ioutil.ReadAll(contents)
		if err != nil {
			return errors.Wrap(err, "failed to read file into memory")
		}

		w, err := b.WriteTemplate(ctx, path, string(byt), data)
		if err != nil {
			return errors.Wrap(err, "failed to write template")
		}

		warnings = append(warnings, w...)

		return nil
	})
}

// determineHeadBranch determines the remote head branch
func (b *Builder) determineHeadBranch(ctx context.Context) (string, error) {
	r, err := git.PlainOpen(b.Dir)
	if err != nil {
		return "", errors.Wrap(err, "failed to open directory as a repository")
	}

	_, err = r.Remote("origin")
	if err != nil {
		// loop through the local branchs
		candidates := []string{"main", "master"}
		for _, branch := range candidates {
			_, err := r.Reference(plumbing.NewBranchReferenceName(branch), true) //nolint:govet
			if err == nil {
				return branch, nil
			}
		}

		// we couldn't find one
		return "", ErrNoHeadBranch
	}

	// we found an origin reference, figure out the HEAD
	cmd := exec.CommandContext(ctx, "git", "remote", "show", "origin")
	cmd.Dir = b.Dir
	out, err := cmd.Output()
	if err != nil {
		return "", errors.Wrap(err, "Failed to get head branch from remote origin")
	}

	matches := headPattern.FindStringSubmatch(string(out))
	if len(matches) != 2 {
		return "", ErrNoRemoteHeadBranch
	}

	return matches[1], nil
}

// makeTemplateParameters creates the map to be provided to the templates.
func (b *Builder) makeTemplateParameters(ctx context.Context) (map[string]interface{}, error) { //nolint:funlen
	headBranch, err := b.determineHeadBranch(ctx)
	if err == ErrNoHeadBranch {
		headBranch = "main"
	} else if err != nil {
		return nil, err
	}

	boxConf, err := box.LoadBox()
	if err != nil {
		return nil, errors.Wrap(err, "failed to load box config")
	}

	return map[string]interface{}{
		"Metadata": map[string]string{
			"Generator": "stencil",
			"Version":   app.Version,
		},

		"Repository": map[string]string{
			"HeadBranch": headBranch,
		},

		"Box": boxConf,

		"Manifest":  b.Manifest,
		"Arguments": b.Manifest.Arguments,
	}, nil
}

// FetchTemplate fetches a template from a git repository
func (b *Builder) FetchTemplate(ctx context.Context, filePath string) (io.Reader, error) {
	f, err := b.GitRepoFs.Open(filePath)
	return f, errors.Wrap(err, filePath)
}

// HasDeviations looks for deviation blocks in a file, returning true if they exist
func (b *Builder) HasDeviations(_ context.Context, filePath string) bool {
	// Search for any commands that are inscribed in the file.
	// Currently we use Block and EndBlock to allow for
	// arbitrary data payloads to be saved across runs of stencil.
	// Eventually we might want to support 3 way merge instead
	f, err := os.Open(filePath)
	if err == nil {
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for i := 0; scanner.Scan(); i++ {
			line := scanner.Text()
			matches := blockPattern.FindStringSubmatch(line)

			// 1: Comment (###|///)
			// 2: Command
			// 3: Argument to the command
			if len(matches) >= 2 {
				cmd := matches[2]
				if strings.EqualFold(cmd, "deviation") {
					return true
				}
			}
		}
	}

	return false
}

// WriteTemplate handles parsing commands (e.g. ///Block) and renders a given template by
// turning it into a functions.RenderedTemplate. This is then written to disk, or skipped
// based on the template's function call. Multiple functions.RenderedTemplates can be returned
// by a single template.
//nolint:funlen,gocyclo,gocritic
func (b *Builder) WriteTemplate(ctx context.Context, filePath,
	contents string, args map[string]interface{}) ([]string, error) {
	// Search for any commands that are inscribed in the file.
	// Currently we use Block and EndBlock to allow for
	// arbitrary data payloads to be saved across runs of stencil.
	// Eventually we might want to support 3 way merge instead
	f, err := os.Open(filePath)
	if err == nil {
		defer f.Close()

		var curBlockName string
		scanner := bufio.NewScanner(f)
		for i := 0; scanner.Scan(); i++ {
			line := scanner.Text()
			matches := blockPattern.FindStringSubmatch(line)
			isCommand := false

			// 1: Comment (###|///)
			// 2: Command
			// 3: Argument to the command
			if len(matches) == 4 {
				cmd := matches[2]
				isCommand = true

				switch cmd {
				case "Block":
					blockName := matches[3]

					if curBlockName != "" {
						return nil, fmt.Errorf("invalid Block when already inside of a block, at %s:%d", filePath, i)
					}
					curBlockName = blockName
				case "EndBlock":
					blockName := matches[3]

					if blockName != curBlockName {
						return nil, fmt.Errorf(
							"invalid EndBlock, found EndBlock with name '%s' while inside of block with name '%s', at %s:%d",
							blockName, curBlockName, filePath, i,
						)
					}

					if curBlockName == "" {
						return nil, fmt.Errorf("invalid EndBlock when not inside of a block, at %s:%d", filePath, i)
					}

					curBlockName = ""
				default:
					isCommand = false
				}
			}

			// we skip lines that had a recognized command in them, or that
			// aren't in a block
			if isCommand || curBlockName == "" {
				continue
			}

			// add the line we processed to the current block we're in
			// and account for having an existing curVal or not. If we
			// don't then we assign curVal to start with the line we
			// just found.
			curVal, ok := args[curBlockName]
			if ok {
				args[curBlockName] = curVal.(string) + "\n" + line
			} else {
				args[curBlockName] = line
			}
		}
	}

	warnings := make([]string, 0)

	if b.HasDeviations(ctx, filePath) {
		warnings = append(warnings, fmt.Sprintf("SKIPPED: '%s' had deviations and will not be re-generated", filePath))
		return warnings, nil
	}

	templates, err := b.renderTemplate(filePath, contents, args)
	if err != nil {
		return nil, err
	}

	for _, renderedTemplate := range templates {
		if len(renderedTemplate.Warnings) > 0 {
			warnings = append(warnings, renderedTemplate.Warnings...)
		}
		if renderedTemplate.Skipped {
			return warnings, nil
		}
		if renderedTemplate.Deleted {
			return warnings, os.RemoveAll(renderedTemplate.Path)
		}
		if renderedTemplate.Path != "" {
			filePath = renderedTemplate.Path
		}

		existingF, err := os.Open(filePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Wrap(err, "failed to open existing file")
		}
		defer existingF.Close()

		existingFile := processors.NewFile(existingF, filePath)
		templateFile := processors.NewFile(renderedTemplate, filePath)

		if existingF != nil {
			processedFile, err := b.Processors.RunDuringCodegen(existingFile, templateFile)
			if err == nil {
				// Use the processor reader instead
				renderedTemplate.Reader = processedFile
			} else if err != nil && err != processors.ErrNotProcessable {
				return nil, errors.Wrap(err, "failed to process file")
			}
		}

		absFilePath := path.Join(b.Dir, filePath)

		action := "Updated"
		if _, err := os.Stat(absFilePath); os.IsNotExist(err) { // nolint: govet,gocritic
			action = "Created"
		}

		perms := os.FileMode(0644)
		if strings.HasSuffix(filePath, ".sh") {
			perms = os.FileMode(0744)
		}
		filePath = strings.TrimSuffix(filePath, ".tpl")

		b.log.Infof("%s file '%s'", action, filePath)
		if err := b.writeFile(filePath, renderedTemplate, perms); err != nil {
			return nil, errors.Wrapf(err, "error creating file '%s'", absFilePath)
		}
	}

	return warnings, nil
}

//nolint:gocritic,funlen
func (b *Builder) renderTemplate(fileName, contents string,
	args map[string]interface{}) ([]*functions.RenderedTemplate, error) {
	srcRendered := &functions.RenderedTemplate{}

	tmpl := template.New(fileName)
	st := functions.NewStencil(tmpl, b.Manifest, srcRendered)

	nargs := make(map[string]interface{})
	for k, v := range args {
		nargs[k] = v
	}

	funcs := functions.Default
	funcs["stencil"] = func() *functions.Stencil { return st }
	funcs["file"] = func() *functions.RenderedTemplate { return st.File }

	funcs["extensions"] = func() *extensions.ExtensionCaller {
		return b.extensionCaller
	}

	tmpl, err := tmpl.Funcs(sprig.TxtFuncMap()).Funcs(funcs).Parse(contents)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, nargs)
	srcRendered.Reader = &buf
	return append(st.Files, srcRendered), err
}

func (b *Builder) writeFile(fileName string, tf io.Reader, perm os.FileMode) error {
	fileName = filepath.Join(b.Dir, fileName)
	err := os.MkdirAll(filepath.Dir(fileName), os.ModePerm)
	if err != nil {
		return err
	}

	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	err = f.Chmod(perm)
	if err != nil {
		return err
	}

	_, err = io.Copy(f, tf)
	return err
}
