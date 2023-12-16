// Copyright 2022 Outreach Corporation. All Rights Reserved.

// Description: This file contains tests for the modules package

package modules_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rgst-io/stencil/internal/modules"
	"github.com/rgst-io/stencil/internal/modules/modulestest"
	"github.com/rgst-io/stencil/pkg/configuration"
	"github.com/sirupsen/logrus"
	"gotest.tools/v3/assert"
)

// newLogger creates a new logger for testing
func newLogger() logrus.FieldLogger {
	log := logrus.New()
	log.SetLevel(logrus.DebugLevel)
	return log
}

func TestCanFetchModule(t *testing.T) {
	ctx := context.Background()
	m, err := modules.New(ctx, "", &configuration.TemplateRepository{Name: "github.com/rgst-io/stencil-base", Version: "main"})
	assert.NilError(t, err, "failed to call New()")

	manifest, err := m.Manifest(ctx)
	assert.NilError(t, err, "failed to call Manifest() on module")
	assert.Assert(t, manifest.Type.Contains(configuration.TemplateRepositoryTypeTemplates), "failed to validate returned manifest")

	fs, err := m.GetFS(ctx)
	assert.NilError(t, err, "failed to call GetFS() on module")

	_, err = fs.Stat("manifest.yaml")
	assert.NilError(t, err, "failed to validate returned manifest from fs")
}

func TestReplacementLocalModule(t *testing.T) {
	sm := &configuration.ServiceManifest{
		Name: "testing-service",
		Modules: []*configuration.TemplateRepository{
			{
				Name: "github.com/rgst-io/stencil-base",
			},
		},
		Replacements: map[string]string{
			"github.com/rgst-io/stencil-base": "file://testdata",
		},
	}

	mods, err := modules.GetModulesForService(context.Background(), &modules.ModuleResolveOptions{ServiceManifest: sm, Log: newLogger()})
	assert.NilError(t, err, "expected GetModulesForService() to not error")
	assert.Equal(t, len(mods), 1, "expected exactly one module to be returned")
	assert.Equal(t, mods[0].URI, sm.Replacements["github.com/rgst-io/stencil-base"],
		"expected module to use replacement URI")
}

func TestCanGetLatestVersion(t *testing.T) {
	ctx := context.Background()
	mods, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		ServiceManifest: &configuration.ServiceManifest{
			Name: "testing-service",
			Modules: []*configuration.TemplateRepository{
				{
					Name: "github.com/rgst-io/stencil-base",
				},
			},
		},
		Log: newLogger(),
	})
	assert.NilError(t, err, "failed to call GetModulesForService()")
	assert.Assert(t, len(mods) >= 1, "expected at least one module to be returned")
}

func TestHandleMultipleConstraints(t *testing.T) {
	ctx := context.Background()
	mods, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		ServiceManifest: &configuration.ServiceManifest{
			Name: "testing-service",
			Modules: []*configuration.TemplateRepository{
				{
					Name:    "github.com/rgst-io/stencil-base",
					Version: "=<0.5.0",
				},
				{
					Name: "nested_constraint",
				},
			},
			Replacements: map[string]string{
				"nested_constraint": "file://testdata/nested_constraint",
			},
		},
		Log: newLogger(),
	})
	assert.NilError(t, err, "failed to call GetModulesForService()")
	assert.Equal(t, len(mods), 2, "expected exactly two modules to be returned")

	// find stencil-base to validate version
	index := -1
	for i, m := range mods {
		if m.Name == "github.com/rgst-io/stencil-base" {
			index = i
			break
		}
	}

	// should resolve to v0.3.2 because testdata wants latest patch of 0.3.0, while we want =<0.5.0
	// which is the latest patch of 0.3.0
	assert.Equal(t, mods[index].Version, "v0.3.2", "expected module to match")
}

func TestHandleNestedModules(t *testing.T) {
	ctx := context.Background()
	mods, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		ServiceManifest: &configuration.ServiceManifest{
			Name: "testing-service",
			Modules: []*configuration.TemplateRepository{
				{
					Name: "a",
				},
			},
			Replacements: map[string]string{
				"a": "file://testdata/nested_modules/a",
				"b": "file://testdata/nested_modules/b",
			},
		},
		Log: newLogger(),
	})
	assert.NilError(t, err, "failed to call GetModulesForService()")

	// ensure that a resolved b
	assert.Equal(t, len(mods), 2, "expected exactly two modules to be returned")

	// ensure that we resolved both a and b
	found := 0
	for _, m := range mods {
		if m.Name == "a" || m.Name == "b" {
			found++
		}
	}

	assert.Equal(t, found, 2, "expected both modules to be returned")
}

func TestFailOnIncompatibleConstraints(t *testing.T) {
	ctx := context.Background()
	_, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		ServiceManifest: &configuration.ServiceManifest{
			Name: "testing-service",
			Modules: []*configuration.TemplateRepository{
				{
					Name:    "github.com/rgst-io/stencil-base",
					Version: ">=0.5.0",
				},
				{
					// wants patch of 0.3.0
					Name: "nested_constraint",
				},
			},
			Replacements: map[string]string{
				"nested_constraint": "file://testdata/nested_constraint",
			},
		},
		Log: newLogger(),
	})
	assert.Error(t, err,
		//nolint:lll // Why: That's the error :(
		"failed to resolve module 'github.com/rgst-io/stencil-base' with constraints\n└─ testing-service (top-level) wants >=0.5.0\n  └─ nested_constraint@v0.0.0-+ wants ~0.3.0\n: no version found matching criteria",
		"expected GetModulesForService() to error")
}

func TestSupportChannelAndConstraint(t *testing.T) {
	ctx := context.Background()
	mods, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		ServiceManifest: &configuration.ServiceManifest{
			Name: "testing-service",
			Modules: []*configuration.TemplateRepository{
				{
					Name:    "github.com/rgst-io/stencil-base",
					Channel: "rc",
					Version: "v0.6.0-rc.4",
				},
			},
		},
		Log: newLogger(),
	})
	assert.NilError(t, err, "failed to call GetModulesForService()")
	assert.Equal(t, len(mods), 1, "expected exactly one module to be returned")
	assert.Equal(t, mods[0].Version, "v0.6.0-rc.4", "expected module to match")
}

func TestCanUseBranch(t *testing.T) {
	ctx := context.Background()
	mods, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		ServiceManifest: &configuration.ServiceManifest{
			Name: "testing-service",
			Modules: []*configuration.TemplateRepository{
				{
					Name:    "github.com/rgst-io/stencil-base",
					Channel: "main",
				},
			},
		},
		Log: newLogger(),
	})
	assert.NilError(t, err, "failed to call GetModulesForService()")

	var mod *modules.Module
	for _, m := range mods {
		if m.Name == "github.com/rgst-io/stencil-base" {
			mod = m
			break
		}
	}
	if mod == nil {
		t.Fatal("failed to find module")
	}

	assert.Equal(t, mod.Version, "main", "expected module to match")
}

func TestBranchAlwaysUsedOverDependency(t *testing.T) {
	ctx := context.Background()

	// Create in-memory module that also requires stencil-base
	man := &configuration.TemplateRepositoryManifest{
		Name: "test",
		Modules: []*configuration.TemplateRepository{
			{
				Name:    "github.com/rgst-io/stencil-base",
				Version: ">=v0.0.0",
			},
		},
	}
	mDep, err := modulestest.NewModuleFromTemplates(man)
	assert.NilError(t, err, "failed to create dep module")

	// Resolve a fake service that requires a branch of a dependency that the in-memory module also requires
	// but with a different version constraint
	mods, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		Replacements: map[string]*modules.Module{"test-dep": mDep},
		ServiceManifest: &configuration.ServiceManifest{
			Name: "testing-service",
			Modules: []*configuration.TemplateRepository{
				{
					Name:    "github.com/rgst-io/stencil-base",
					Version: "main",
				},
				{
					Name: "test-dep",
				},
			},
		},
		Log: newLogger(),
	})
	assert.NilError(t, err, "failed to call GetModulesForService()")

	var mod *modules.Module
	for _, m := range mods {
		if m.Name == "github.com/rgst-io/stencil-base" {
			mod = m
			break
		}
	}
	if mod == nil {
		t.Fatal("failed to find module")
	}

	assert.Equal(t, mod.Version, "main", "expected module to match")
}

func TestCanRespectChannels(t *testing.T) {
	t.Skip("Breaks when a module isn't currently on an rc version")
	ctx := context.Background()
	mods, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		ServiceManifest: &configuration.ServiceManifest{
			Name: "testing-service",
			Modules: []*configuration.TemplateRepository{
				{
					Name:    "github.com/rgst-io/stencil-base",
					Channel: "rc",
				},
				{
					Name: "github.com/rgst-io/stencil-base",
				},
			},
		},
		Log: newLogger(),
	})
	assert.NilError(t, err, "failed to call GetModulesForService()")
	assert.Equal(t, len(mods), 1, "expected exactly one module to be returned")
	if !strings.Contains(mods[0].Version, "-rc.") {
		t.Fatalf("expected module to be an RC, but got %s", mods[0].Version)
	}
}

func TestShouldResolveInMemoryModule(t *testing.T) {
	ctx := context.Background()

	// require test-dep which is also an in-memory module to make sure that we can resolve at least once
	// an in-memory module

	man := &configuration.TemplateRepositoryManifest{
		Name: "test",
		Modules: []*configuration.TemplateRepository{
			{Name: "test-dep"},
		},
	}
	m, err := modulestest.NewModuleFromTemplates(man)
	assert.NilError(t, err, "failed to create module")

	// this relies on the top-level to ensure that re-resolving still picks
	// the in-memory module
	man = &configuration.TemplateRepositoryManifest{
		Name: "test-dep",
		Modules: []*configuration.TemplateRepository{
			{Name: "test"},
		},
	}
	mDep, err := modulestest.NewModuleFromTemplates(man)
	assert.NilError(t, err, "failed to create dep module")

	mods, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		Module:       m,
		Replacements: map[string]*modules.Module{"test-dep": mDep},
		Log:          newLogger(),
	})
	assert.NilError(t, err, "failed to call GetModulesForService()")
	assert.Equal(t, len(mods), 2, "expected exactly two modules to be returned")

	var mod *modules.Module
	for _, m := range mods {
		if m.Name == "test" {
			mod = m
			break
		}
	}
	assert.Equal(t, mod.Name, m.Name, "expected module to match")
}

func TestShouldErrorOnTwoDifferentChannels(t *testing.T) {
	ctx := context.Background()
	_, err := modules.GetModulesForService(ctx, &modules.ModuleResolveOptions{
		ServiceManifest: &configuration.ServiceManifest{
			Name: "testing-service",
			Modules: []*configuration.TemplateRepository{
				{
					Name:    "github.com/rgst-io/stencil-base",
					Channel: "rc",
				},
				{
					Name:    "github.com/rgst-io/stencil-base",
					Channel: "unstable",
				},
			},
		},
		Log: newLogger(),
	})
	assert.ErrorContains(t, err, "previously resolved with channel", "expected GetModulesForService() to error")
}
