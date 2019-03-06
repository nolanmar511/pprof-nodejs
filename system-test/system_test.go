// Copyright 2019 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package testing

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/google/pprof/profile"
)

var (
	runOnlyV8CanaryTest = flag.Bool("run_only_v8_canary_test", false, "if true test will be run only with the v8-canary build, otherwise, no tests will be run with v8 canary")
	binaryHost          = flag.String("binary_host", "", "host from which to download precompiled binaries; if no value is specified, binaries will be built from source.")
	pprofNodejsPath     = flag.String("pprof_nodejs_path", "", "path to directory containing pprof-nodejs module")

	runID             = strings.Replace(time.Now().Format("2006-01-02-15-04-05.000000-0700"), ".", "-", -1)
	benchFinishString = "busybench finished profiling"
	errorString       = "failed to set up or run the benchmark"
)

var tmpl = template.Must(template.New("benchTemplate").Parse(`
#! /bin/bash
(

retry() {
  for i in {1..3}; do
    "${@}" && return 0
  done
  return 1
}

# Display commands being run
set -x

# Note directory from which test is being run.
BASE_DIR=$(pwd)

# Install nvm
retry curl -o- https://raw.githubusercontent.com/creationix/nvm/v0.33.8/install.sh | bash >/dev/null
export NVM_DIR="$HOME/.nvm" >/dev/null
[ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh" >/dev/null

# Install desired version of Node.JS.
# nvm install writes to stderr and stdout on successful install, so both are
# redirected.
{{if .NVMMirror}}NVM_NODEJS_ORG_MIRROR={{.NVMMirror}}{{end}} retry nvm install {{.NodeVersion}} &>/dev/null
node -v

# Build and pack pprof module
cd {{.PProfNodeJSPath}}

# TODO: remove this workaround.
# For v8-canary tests, we need to use the version of NAN on github, which 
# contains unreleased fixes which allows the native component to be compiled
# with Node 11.
{{if .NVMMirror}} retry npm install https://github.com/nodejs/nan.git {{end}}

retry npm install --nodedir="$NODEDIR" >/dev/null
npm run compile
npm pack >/dev/null
VERSION=$(node -e "console.log(require('./package.json').version);")
PROFILER="{{.PProfNodeJSPath}}/pprof-$VERSION.tgz"

# Create and set up directory for running benchmark.
TESTDIR="$BASE_DIR/{{.Name}}"
mkdir -p "$TESTDIR"
cp -r "$BASE_DIR/busybench" "$TESTDIR"
cd "$TESTDIR/busybench"

retry npm install pify @types/pify typescript gts @types/node >/dev/null
retry npm install "$PROFILER" >/dev/null
retry npm install --nodedir="$NODEDIR" --build-from-source=pprof "$PROFILER"
npm run compile

# Run benchmark with agent
node -v
node --trace-warnings build/src/busybench.js 30

echo $PWD
ls

) 2>&1 | while read line; do echo "$(date): ${line}"; done >&1
`))

type profileSummary struct {
	profileType  string
	functionName string
	sourceFile   string
	lineNumber   int64
}

type pprofTestCase struct {
	name         string
	nodeVersion  string
	nvmMirror    string
	wantProfiles []profileSummary
	script       string
}

func (tc *pprofTestCase) generateScript(tmpl *template.Template) (string, error) {
	var buf bytes.Buffer
	err := tmpl.Execute(&buf,
		struct {
			Name            string
			NodeVersion     string
			NVMMirror       string
			FinishString    string
			ErrorString     string
			BinaryHost      string
			DurationSec     int
			PProfNodeJSPath string
		}{
			Name:            tc.name,
			NodeVersion:     tc.nodeVersion,
			NVMMirror:       tc.nvmMirror,
			FinishString:    benchFinishString,
			ErrorString:     errorString,
			BinaryHost:      *binaryHost,
			DurationSec:     10,
			PProfNodeJSPath: *pprofNodejsPath,
		})
	filename := fmt.Sprintf("%s.sh", tc.name)
	err = ioutil.WriteFile(filename, buf.Bytes(), os.ModePerm)
	if err != nil {
		return "", fmt.Errorf("failed to render startup script for %s: %v", tc.name, err)
	}

	return filename, nil
}

func TestAgentIntegration(t *testing.T) {
	wantProfiles := []profileSummary{
		// {profileType: "time", functionName: "busyLoop", sourceFile: "busybench.ts"},
		{profileType: "heap", functionName: "benchmark", sourceFile: "busybench.ts"},
	}

	testcases := []pprofTestCase{
		{
			name:         fmt.Sprintf("pprof-node6-%s", runID),
			wantProfiles: wantProfiles,
			nodeVersion:  "6",
		},
		{
			name:         fmt.Sprintf("pprof-node8-%s", runID),
			wantProfiles: wantProfiles,
			nodeVersion:  "8",
		},
		{
			name:         fmt.Sprintf("pprof-node10-%s", runID),
			wantProfiles: wantProfiles,
			nodeVersion:  "10",
		},
		{
			name:         fmt.Sprintf("pprof-node11-%s", runID),
			wantProfiles: wantProfiles,
			nodeVersion:  "11",
		},
	}
	if *runOnlyV8CanaryTest {
		testcases = []pprofTestCase{{
			name:         fmt.Sprintf("pprof-v8-canary-%s", runID),
			wantProfiles: wantProfiles,
			nodeVersion:  "node", // install latest version of node
			nvmMirror:    "https://nodejs.org/download/v8-canary",
		}}
	}

	// Prevent test cases from running in parallel.
	runtime.GOMAXPROCS(1)

	for _, tc := range testcases {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {

			bench, err := tc.generateScript(tmpl)
			if err != nil {
				t.Fatalf("failed to initialize bench script: %v", err)
			}

			cmd := exec.Command("/bin/sh", bench)
			var testOut bytes.Buffer
			cmd.Stdout = &testOut
			err = cmd.Run()
			t.Log(testOut.String())
			if err != nil {
				t.Fatalf("failed to execute benchmark: %v", err)
			}

			for _, wantProfile := range tc.wantProfiles {
				profilePath := fmt.Sprintf("%s/busybench/%s.pb.gz", tc.name, wantProfile.profileType)
				if err := checkProfile(profilePath, wantProfile); err != nil {
					t.Errorf("failed to collect expected %s profile %s: %v", wantProfile.profileType, profilePath, err)
				}
			}
		})
	}
}

func checkProfile(profilePath string, wantProfile profileSummary) error {
	f, err := os.Open(profilePath)
	if err != nil {
		return fmt.Errorf("failed to open profile: %v", err)
	}

	pr, err := profile.Parse(f)
	if err != nil {
		return fmt.Errorf("failed to parse profile: %v", err)
	}

	// Check sample types
	/*
		switch wantProfile.profileType {
		case "WALL":
		case "HEAP":
		default:
			fmt.Errorf("unrecognized profile type %q", wantProfile.profileType)
		}
	*/

	var locFound bool
OUTER:
	for _, loc := range pr.Location {
		for _, line := range loc.Line {
			if wantProfile.functionName == line.Function.Name && (wantProfile.lineNumber == line.Line || wantProfile.lineNumber == 0) {
				locFound = true
				break OUTER
			}
		}
	}
	if !locFound {
		return fmt.Errorf("Location (function: %s, line: %d) not found in profiles of type %s", wantProfile.functionName, wantProfile.lineNumber, wantProfile.profileType)
	}
	return nil
}
