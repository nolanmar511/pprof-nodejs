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
	"archive/tar"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/pprof/profile"
)

var (
	binaryHost          = flag.String("binary_host", "", "host from which to download precompiled binaries; if no value is specified, binaries will be built from source.")
	dockerfile          = flag.String("dockerfile", "", "path to docker file; if unspecified, system test won't be run in docker.")
	runOnlyV8CanaryTest = flag.Bool("run_only_v8_canary_test", false, "if true test will be run only with the v8-canary build, otherwise, no tests will be run with v8 canary build")
	pprofDir            = flag.String("pprof_nodejs_path", "", "path to directory containing pprof-nodejs module")

	runID = strings.Replace(time.Now().Format("2006-01-02-15-04-05.000000-0700"), ".", "-", -1)
)

const alpineDocker = `FROM node:10-alpine
RUN apk add --no-cache python curl bash build-base`

var tmpl = template.Must(template.New("benchTemplate").Parse(`
#! /bin/bash
(

retry() {
  for i in {1..3}; do
    "${@}" && return 0
  done
  return 1
}

# Display commands being run.
set -x

# Note directory from which test is being run.
BASE_DIR=$(pwd)

# Install desired version of Node.JS.
# nvm install writes to stderr and stdout on successful install, so both are
# redirected.
. ~/.nvm/nvm.sh &>/dev/null # load nvm.
{{if .NVMMirror}}NVM_NODEJS_ORG_MIRROR={{.NVMMirror}}{{end}} retry nvm install {{.NodeVersion}} &>/dev/null

NODEDIR=$(dirname $(dirname $(which node)))

# Build and pack pprof module.
cd {{.PprofDir}}

# TODO: remove this workaround when a new version of nan (current version 
#       2.12.1) is released.
# For v8-canary tests, we need to use the version of NAN on github, which 
# contains unreleased fixes that allow the native component to be compiled
# with Node's V8 canary build.
{{if .NVMMirror}} retry npm install https://github.com/nodejs/nan.git {{end}} >/dev/null

retry npm install --nodedir="$NODEDIR" {{if .BinaryHost}}--fallback-to-build=false --pprof_binary_host_mirror={{.BinaryHost}}{{end}} >/dev/null

npm run compile
npm pack >/dev/null
VERSION=$(node -e "console.log(require('./package.json').version);")
PROFILER="{{.PprofDir}}/pprof-$VERSION.tgz"

# Create and set up directory for running benchmark.
TESTDIR="$BASE_DIR/{{.Name}}"
mkdir -p "$TESTDIR"
cp -r "$BASE_DIR/busybench" "$TESTDIR"
cd "$TESTDIR/busybench"

retry npm install pify @types/pify typescript gts @types/node >/dev/null
retry npm install --nodedir="$NODEDIR" {{if .BinaryHost}}--fallback-to-build=false --pprof_binary_host_mirror={{.BinaryHost}}{{end}} "$PROFILER" >/dev/null

npm run compile >/dev/null

# Run benchmark, which will collect and save profiles.
node -v
node --trace-warnings build/src/busybench.js {{.DurationSec}}

# Write all output standard out with timestamp.
) 2>&1 | while read line; do echo "$(date): ${line}"; done >&1
`))

type profileSummary struct {
	profileType  string
	functionName string
	sourceFile   string
}

type pprofTestCase struct {
	name         string
	nodeVersion  string
	nvmMirror    string
	wantProfiles []profileSummary
}

func (tc *pprofTestCase) generateScript(tmpl *template.Template) (string, error) {
	var buf bytes.Buffer
	err := tmpl.Execute(&buf,
		struct {
			Name        string
			NodeVersion string
			NVMMirror   string
			DurationSec int
			PprofDir    string
			BinaryHost  string
		}{
			Name:        tc.name,
			NodeVersion: tc.nodeVersion,
			NVMMirror:   tc.nvmMirror,
			DurationSec: 10,
			PprofDir:    *pprofDir,
			BinaryHost:  *binaryHost,
		})
	if err != nil {
		return "", fmt.Errorf("failed to render benchmark script for %s: %v", tc.name, err)
	}
	filename := fmt.Sprintf("%s.sh", tc.name)
	if err := ioutil.WriteFile(filename, buf.Bytes(), os.ModePerm); err != nil {
		return "", fmt.Errorf("failed to write benchmark script for %s to %s: %v", tc.name, filename, err)
	}
	return filename, nil
}

func TestAgentIntegration(t *testing.T) {
	wantProfiles := []profileSummary{
		{profileType: "time", functionName: "busyLoop", sourceFile: "busybench.js"},
		{profileType: "heap", functionName: "benchmark", sourceFile: "busybench.js"},
	}

	testcases := []pprofTestCase{
		{
			name:         fmt.Sprintf("pprof-node6-%s", runID),
			wantProfiles: wantProfiles,
			nodeVersion:  "6",
		},
		/*
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
		*/
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

	var cli *client.Client
	ctx := context.Background()
	if *dockerfile != "" {
		var err error
		if cli, err = client.NewClientWithOpts(client.WithVersion("1.37")); err != nil {
			t.Fatalf("failed to create docker client: %v", err)
		}
		buildCtx, err := getDockerfileToTar(alpineDocker)
		if err != nil {
			t.Fatalf("failed to get docker build context: %v", err)
		}
		imgRsp, err := cli.ImageBuild(ctx, buildCtx, types.ImageBuildOptions{
			Tags: []string{"test-image"},
		})
		if err != nil {
			t.Fatalf("failed to build docker image: %v", err)
		}
		io.Copy(os.Stdout, imgRsp.Body)
		defer imgRsp.Body.Close()
	}

	for _, tc := range testcases {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			bench, err := tc.generateScript(tmpl)
			if err != nil {
				t.Fatalf("failed to initialize bench script: %v", err)
			}

			if err != nil {
				t.Fatalf("failed to build docker image: %v", err)
			}

			if *dockerfile == "" {
				cmd := exec.Command("/bin/bash", bench)
				var testOut bytes.Buffer
				cmd.Stdout = &testOut
				err = cmd.Run()
				t.Log(testOut.String())
				if err != nil {
					t.Fatalf("failed to execute benchmark: %v", err)
				}
			} else {

				pwd, err := os.Getwd()
				if err != nil {
					t.Fatalf("failed to get workind directory: %v", err)
				}
				benchPath, err := filepath.Abs(bench)
				if err != nil {
					t.Fatalf("failed to get absolute path of %s: %v", benchPath, err)
				}

				resp, err := cli.ContainerCreate(ctx, &container.Config{
					Image: "test-image",
					Cmd:   []string{"/bin/bash"},
					// Cmd:     []string{"ls", pwd, "-R"},
					Tty:     true,
					Volumes: map[string]struct{}{fmt.Sprintf("%s:%s", pwd, pwd): {}},
				}, nil, nil, "")
				if err != nil {
					t.Fatalf("failed to created docker container: %v", err)
				}
				fmt.Printf("Created container: %v\n", resp)

				if err := cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
					t.Fatalf("failed to start container: %v", err)
				}
				fmt.Printf("Started container: %v\n", resp)

				out, err := cli.ContainerLogs(ctx, resp.ID, types.ContainerLogsOptions{ShowStdout: true})
				if err != nil {
					t.Fatalf("failed to get container logs: %v", err)
				}
				fmt.Println("Container logs")
				io.Copy(os.Stdout, out)

				statusCh, errCh := cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)

				fmt.Println("Waiting for container")
				select {
				case err := <-errCh:
					if err != nil {
						t.Fatalf("failed to wait for container: %v", err)
					}
				case <-statusCh:
				}

				fmt.Println("Finished waiting for container")

			}

			for _, wantProfile := range tc.wantProfiles {
				profilePath := fmt.Sprintf("%s/busybench/%s.pb.gz", tc.name, wantProfile.profileType)
				if err := checkProfile(profilePath, wantProfile); err != nil {
					t.Errorf("failed to collect expected %s profile: %v", wantProfile.profileType, err)
				}
			}
		})
	}
}

func getDockerfileToTar(dockerfile string) (io.Reader, error) {
	/*
		r, err := os.Open(dockerfile)
		if err != nil {
			return nil, fmt.Errorf("failed to open docker file %s: %v", dockerfile, err)
		}
		f, err := ioutil.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("failed to read docker file %s: %v", dockerfile, err)
		}
	*/

	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	defer w.Close()

	fmt.Println(dockerfile)

	if err := w.WriteHeader(&tar.Header{Name: "Dockerfile", Size: int64(len(dockerfile))}); err != nil {
		return nil, fmt.Errorf("failed to write tar header: %v", err)
	}
	if _, err := w.Write([]byte(dockerfile)); err != nil {
		return nil, fmt.Errorf("failed to write dockerfile to tar: %v", err)
	}

	return bytes.NewReader(buf.Bytes()), nil
}

func checkProfile(path string, want profileSummary) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open profile: %v", err)
	}

	pr, err := profile.Parse(f)
	if err != nil {
		return fmt.Errorf("failed to parse profile: %v", err)
	}

	for _, loc := range pr.Location {
		for _, line := range loc.Line {
			if want.functionName == line.Function.Name && strings.HasSuffix(line.Function.Filename, want.sourceFile) {
				return nil
			}
		}
	}
	return fmt.Errorf("Location (function: %s, file: %s) not found in profiles of type %s", want.functionName, want.sourceFile, want.profileType)
}
