package build

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"

	"github.com/windmilleng/tilt/internal/container"
	"github.com/windmilleng/tilt/internal/docker"
	"github.com/windmilleng/tilt/internal/dockerfile"
	"github.com/windmilleng/tilt/internal/testutils"
	"github.com/windmilleng/tilt/pkg/model"
)

const simpleDockerfile = dockerfile.Dockerfile("FROM alpine")

func TestDigestAsTag(t *testing.T) {
	dig := digest.Digest("sha256:cc5f4c463f81c55183d8d737ba2f0d30b3e6f3670dbe2da68f0aac168e93fbb1")
	tag, err := digestAsTag(dig)
	if err != nil {
		t.Fatal(err)
	}

	expected := "tilt-cc5f4c463f81c551"
	if tag != expected {
		t.Errorf("Expected %s, actual: %s", expected, tag)
	}
}

func TestDigestMatchesRef(t *testing.T) {
	dig := digest.Digest("sha256:cc5f4c463f81c55183d8d737ba2f0d30b3e6f3670dbe2da68f0aac168e93fbb1")
	tag, err := digestAsTag(dig)
	if err != nil {
		t.Fatal(err)
	}

	ref, _ := container.ParseNamedTagged("windmill.build/image:" + tag)
	if !digestMatchesRef(ref, dig) {
		t.Errorf("Expected digest %s to match ref %s", dig, ref)
	}
}

func TestDigestNotMatchesRef(t *testing.T) {
	dig := digest.Digest("sha256:cc5f4c463f81c55183d8d737ba2f0d30b3e6f3670dbe2da68f0aac168e93fbb1")
	ref, _ := container.ParseNamedTagged("windmill.build/image:tilt-deadbeef")
	if digestMatchesRef(ref, dig) {
		t.Errorf("Expected digest %s to not match ref %s", dig, ref)
	}
}

func TestDigestAsTagToShort(t *testing.T) {
	dig := digest.Digest("sha256:cc")
	_, err := digestAsTag(dig)
	expected := "too short"
	if err == nil || !strings.Contains(err.Error(), expected) {
		t.Errorf("expected error %q, actual: %v", expected, err)
	}
}

func TestDigestFromSingleStepOutput(t *testing.T) {
	f := newFakeDockerBuildFixture(t)
	defer f.teardown()

	input := docker.ExampleBuildOutput1
	expected := digest.Digest("sha256:11cd0b38bc3ceb958ffb2f9bd70be3fb317ce7d255c8a4c3f4af30e298aa1aab")
	actual, err := f.b.getDigestFromBuildOutput(f.ctx, bytes.NewBuffer([]byte(input)))
	if err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Errorf("Expected %s, got %s", expected, actual)
	}
}

func TestDigestFromOutputV1_23(t *testing.T) {
	f := newFakeDockerBuildFixture(t)
	defer f.teardown()

	input := docker.ExampleBuildOutputV1_23
	expected := digest.Digest("sha256:11cd0eb38bc3ceb958ffb2f9bd70be3fb317ce7d255c8a4c3f4af30e298aa1aab")
	f.fakeDocker.Images["11cd0b38bc3c"] = types.ImageInspect{ID: string(expected)}
	actual, err := f.b.getDigestFromBuildOutput(f.ctx, bytes.NewBuffer([]byte(input)))
	if err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Errorf("Expected %s, got %s", expected, actual)
	}
}

func TestConditionalRunInFakeDocker(t *testing.T) {
	f := newFakeDockerBuildFixture(t)
	defer f.teardown()

	f.WriteFile("a.txt", "a")
	f.WriteFile("b.txt", "b")

	s := model.Sync{
		LocalPath:     f.Path(),
		ContainerPath: "/src",
	}
	run1 := model.Run{
		Cmd:      model.ToShellCmd("cat /src/a.txt > /src/c.txt"),
		Triggers: model.NewPathSet([]string{"a.txt"}, f.Path()),
	}
	run2 := model.Run{
		Cmd: model.ToShellCmd("cat /src/b.txt > /src/d.txt"),
	}

	_, err := f.b.DeprecatedFastBuildImage(f.ctx, f.ps, f.getNameFromTest(), simpleDockerfile, []model.Sync{s}, model.EmptyMatcher, []model.Run{run1, run2}, model.Cmd{})
	if err != nil {
		t.Fatal(err)
	}

	expected := expectedFile{
		Path: "Dockerfile",
		Contents: `FROM alpine
COPY /src/a.txt /src/a.txt
RUN cat /src/a.txt > /src/c.txt
ADD . /
RUN cat /src/b.txt > /src/d.txt
LABEL "tilt.buildMode"="scratch"
LABEL "tilt.test"="1"`,
	}
	testutils.AssertFileInTar(f.t, tar.NewReader(f.fakeDocker.BuildOptions.Context), expected)
}

func TestAllConditionalRunsInFakeDocker(t *testing.T) {
	f := newFakeDockerBuildFixture(t)
	defer f.teardown()

	f.WriteFile("a.txt", "a")
	f.WriteFile("b.txt", "b")

	s := model.Sync{
		LocalPath:     f.Path(),
		ContainerPath: "/src",
	}
	run1 := model.Run{
		Cmd:      model.ToShellCmd("cat /src/a.txt > /src/c.txt"),
		Triggers: model.NewPathSet([]string{"a.txt"}, f.Path()),
	}

	_, err := f.b.DeprecatedFastBuildImage(f.ctx, f.ps, f.getNameFromTest(), simpleDockerfile, []model.Sync{s}, model.EmptyMatcher, []model.Run{run1}, model.Cmd{})
	if err != nil {
		t.Fatal(err)
	}

	expected := expectedFile{
		Path: "Dockerfile",
		Contents: `FROM alpine
COPY /src/a.txt /src/a.txt
RUN cat /src/a.txt > /src/c.txt
ADD . /
LABEL "tilt.buildMode"="scratch"
LABEL "tilt.test"="1"`,
	}
	testutils.AssertFileInTar(f.t, tar.NewReader(f.fakeDocker.BuildOptions.Context), expected)
}

func makeDockerBuildErrorOutput(s string) string {
	b := &bytes.Buffer{}
	err := json.NewEncoder(b).Encode(s)
	if err != nil {
		panic(err)
	}

	return fmt.Sprintf(`{"errorDetail":{"message":%s},"error":%s}`, b.String(), b.String())
}

func TestCleanUpBuildKitErrors(t *testing.T) {
	for _, tc := range []struct {
		buildKitError     string
		expectedTiltError string
	}{
		// actual error currently emitted by buildkit when a `RUN` fails
		{
			//nolint
			buildKitError:     "failed to solve with frontend dockerfile.v0: failed to build LLB: executor failed running [/bin/sh -c go install github.com/windmilleng/servantes/vigoda]: runc did not terminate sucessfully",
			expectedTiltError: "executor failed running [/bin/sh -c go install github.com/windmilleng/servantes/vigoda]",
		},
		//nolint
		// artificial error - in case docker for some reason doesn't have "executor failed running", don't trim "runc did not terminate sucessfully"
		{
			//nolint
			buildKitError:     "failed to solve with frontend dockerfile.v0: failed to build LLB: [/bin/sh -c go install github.com/windmilleng/servantes/vigoda]: runc did not terminate sucessfully",
			expectedTiltError: "[/bin/sh -c go install github.com/windmilleng/servantes/vigoda]: runc did not terminate sucessfully",
		},
		// actual error currently emitted by buildkit when an `ADD` file is missing
		{
			buildKitError:     `failed to solve with frontend dockerfile.v0: failed to build LLB: failed to compute cache key: "/foo.txt" not found: not found`,
			expectedTiltError: `"/foo.txt" not found`,
		},
		// artificial error - in case docker fails to emit the double "not found", don't trim the one at the end
		// output in this case could do without the "failed to compute cache key", but this test is just ensuring we
		// err on the side of caution, rather than that we're emitting an optimal message for an artificial error
		{
			buildKitError:     `failed to solve with frontend dockerfile.v0: failed to build LLB: failed to compute cache key: "/foo.txt": not found`,
			expectedTiltError: `failed to compute cache key: "/foo.txt": not found`,
		},
		// artificial error - in case docker doesn't say "not found" at all
		{
			buildKitError:     `failed to solve with frontend dockerfile.v0: failed to build LLB: failed to compute cache key: "/foo.txt"`,
			expectedTiltError: `failed to compute cache key: "/foo.txt"`,
		},
		// check an unanticipated error that still has the annoying preamble
		{
			buildKitError:     "failed to solve with frontend dockerfile.v0: failed to build LLB: who knows, some made up explosion",
			expectedTiltError: "who knows, some made up explosion",
		},
	} {
		t.Run(tc.expectedTiltError, func(t *testing.T) {
			f := newFakeDockerBuildFixture(t)
			defer f.teardown()

			ctx, _, _ := testutils.CtxAndAnalyticsForTest()
			s := makeDockerBuildErrorOutput(tc.buildKitError)
			_, err := f.b.getDigestFromBuildOutput(ctx, strings.NewReader(s))
			require.NotNil(t, err)
			require.Equal(t, fmt.Sprintf("ImageBuild: %s", tc.expectedTiltError), err.Error())
		})
	}
}
