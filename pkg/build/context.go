/*
Copyright 2021 Google LLC All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	gb "go/build"
	"io"
	"log"
	"os/exec"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	gorootWarningTemplate = `NOTICE!
-----------------------------------------------------------------
ko and go have mismatched GOROOT:
    go/build.Default.GOROOT = %q
    $(go env GOROOT) = %q

Inferring GOROOT=%q

Run this to remove this warning:
    export GOROOT=$(go env GOROOT)

For more information see:
    https://github.com/google/ko/issues/106
-----------------------------------------------------------------
`
)

// https://golang.org/pkg/cmd/go/internal/modinfo/#ModulePublic
type modules struct {
	main *modInfo
	deps map[string]*modInfo
}

type modInfo struct {
	Path string
	Dir  string
	Main bool
}

type buildContext interface {
	importPackage(path string, srcDir string) (*gb.Package, error)
	moduleInfo(ctx context.Context) (*modules, error)
	qualifyLocalImport(importpath string) (string, error)
}

type goBuildContext struct {
	// Not a pointer, we need a copy of the go/build.Context. Otherwise, we'll
	// end up with race conditions for parallel image builds.
	bc gb.Context
}

// newBuildContext creates a new buildContext, which wraps a go/build.Context.
func newBuildContext(ctx context.Context, dir string) (buildContext, error) {
	g := &goBuildContext{
		bc: gb.Default,
	}
	g.bc.Dir = dir

	// If $(go env GOROOT) successfully returns a non-empty string that differs from
	// the default build context GOROOT, print a warning and use $(go env GOROOT).
	goroot, err := getGoroot(ctx, dir)
	if err != nil {
		// On error, print the output and set goroot to "" to avoid using it later.
		log.Printf("Unexpected error running \"go env GOROOT\": %v\n%v", err, goroot)
		goroot = ""
	} else if goroot == "" {
		log.Printf(`Unexpected: $(go env GOROOT) == ""`)
	}
	if goroot != "" && g.bc.GOROOT != goroot {
		log.Printf(gorootWarningTemplate, g.bc.GOROOT, goroot, goroot)
		g.bc.GOROOT = goroot
	}

	return g, nil
}

// importPackage wraps go/build.Context Import()
func (g *goBuildContext) importPackage(path string, srcDir string) (*gb.Package, error) {
	return g.bc.Import(path, srcDir, gb.ImportComment)
}

// moduleInfo returns the module path and module root directory for a project
// using go modules, otherwise returns nil.
//
// Related: https://github.com/golang/go/issues/26504
func (g *goBuildContext) moduleInfo(ctx context.Context) (*modules, error) {
	modules := modules{
		deps: make(map[string]*modInfo),
	}

	// TODO we read all the output as a single byte array - it may
	// be possible & more efficient to stream it
	cmd := exec.CommandContext(ctx, "go", "list", "-mod=readonly", "-json", "-m", "all")
	cmd.Dir = g.bc.Dir
	output, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	dec := json.NewDecoder(bytes.NewReader(output))

	for {
		var info modInfo

		err := dec.Decode(&info)
		if err == io.EOF {
			// all done
			break
		}

		modules.deps[info.Path] = &info

		if info.Main {
			modules.main = &info
		}

		if err != nil {
			return nil, fmt.Errorf("error reading module data %w", err)
		}
	}

	if modules.main == nil {
		return nil, fmt.Errorf("couldn't find main module")
	}

	return &modules, nil
}

func (g *goBuildContext) qualifyLocalImport(importpath string) (string, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName,
		Dir:  g.bc.Dir,
	}
	pkgs, err := packages.Load(cfg, importpath)
	if err != nil {
		return "", err
	}
	if len(pkgs) != 1 {
		return "", fmt.Errorf("found %d local packages, expected 1", len(pkgs))
	}
	return pkgs[0].PkgPath, nil
}

// getGoroot shells out to `go env GOROOT` to determine
// the GOROOT for the installed version of go so that we
// can set it in our buildContext. By default, the GOROOT
// of our buildContext is set to the GOROOT at install
// time for `ko`, which means that we break when certain
// package managers update go or when using a pre-built
// `ko` binary that expects a different GOROOT.
//
// See https://github.com/google/ko/issues/106
func getGoroot(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOROOT")
	// It may not necessary to set the command working directory here,
	// but it helps keep everything consistent.
	cmd.Dir = dir
	output, err := cmd.Output()
	return strings.TrimSpace(string(output)), err
}
