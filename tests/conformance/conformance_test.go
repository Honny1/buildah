package conformance

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/containers/buildah"
	"github.com/containers/buildah/copier"
	"github.com/containers/buildah/define"
	"github.com/containers/buildah/imagebuildah"
	"github.com/containers/buildah/internal/config"
	"github.com/containers/image/v5/docker/daemon"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/compression"
	is "github.com/containers/image/v5/storage"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/transports"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/ioutils"
	"github.com/containers/storage/pkg/reexec"
	dockertypes "github.com/docker/docker/api/types"
	dockerdockerclient "github.com/docker/docker/client"
	docker "github.com/fsouza/go-dockerclient"
	digest "github.com/opencontainers/go-digest"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/openshift/imagebuilder"
	"github.com/openshift/imagebuilder/dockerclient"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"
)

const (
	// See http://pubs.opengroup.org/onlinepubs/9699919799/utilities/pax.html#tag_20_92_13_06, from archive/tar
	cISUID = 04000 // Set uid, from archive/tar
	cISGID = 02000 // Set gid, from archive/tar
	cISVTX = 01000 // Save text (sticky bit), from archive/tar
)

var (
	originalSkip = []string{
		"created",
		"container",
		"docker_version",
		"container_config:hostname",
		"config:hostname",
		"config:image",
		"container_config:cmd",
		"container_config:image",
		"history",
		"rootfs:diff_ids",
		"moby.buildkit.buildinfo.v1",
	}
	ociSkip = []string{
		"created",
		"history",
		"rootfs:diff_ids",
	}
	fsSkip = []string{
		// things that we volume mount or synthesize for RUN statements that currently bleed through
		"(dir):etc:mtime",
		"(dir):etc:(dir):hosts",
		"(dir):etc:(dir):resolv.conf",
		"(dir):run",
		"(dir):run:mtime",
		"(dir):run:(dir):.containerenv",
		"(dir):run:(dir):secrets",
		"(dir):proc",
		"(dir):proc:mtime",
		"(dir):sys",
		"(dir):sys:mtime",
	}
	testDate            = time.Unix(1485449953, 0)
	compareLayers       = false
	compareImagebuilder = false
	testDataDir         = ""
	dockerDir           = ""
	imagebuilderDir     = ""
	buildahDir          = ""
	contextCanDoXattrs  *bool
	storageCanDoXattrs  *bool
)

func TestMain(m *testing.M) {
	var logLevel string
	if reexec.Init() {
		return
	}

	cwd, err := os.Getwd()
	if err != nil {
		logrus.Fatalf("error finding current directory: %v", err)
	}
	testDataDir = filepath.Join(cwd, "testdata")

	flag.StringVar(&logLevel, "log-level", "error", "buildah logging log level")
	flag.BoolVar(&compareLayers, "compare-layers", compareLayers, "compare instruction-by-instruction")
	flag.BoolVar(&compareImagebuilder, "compare-imagebuilder", compareImagebuilder, "also compare using imagebuilder")
	flag.StringVar(&testDataDir, "testdata", testDataDir, "location of conformance testdata")
	flag.StringVar(&dockerDir, "docker-dir", dockerDir, "location to save docker build results")
	flag.StringVar(&imagebuilderDir, "imagebuilder-dir", imagebuilderDir, "location to save imagebuilder build results")
	flag.StringVar(&buildahDir, "buildah-dir", buildahDir, "location to save buildah build results")
	flag.Parse()
	var tempdir string
	if buildahDir == "" || dockerDir == "" || imagebuilderDir == "" {
		if tempdir == "" {
			if tempdir, err = os.MkdirTemp("", "conformance"); err != nil {
				logrus.Fatalf("creating temporary directory: %v", err)
				os.Exit(1)
			}
		}
	}
	if buildahDir == "" {
		buildahDir = filepath.Join(tempdir, "buildah")
	}
	if dockerDir == "" {
		dockerDir = filepath.Join(tempdir, "docker")
	}
	if imagebuilderDir == "" {
		imagebuilderDir = filepath.Join(tempdir, "imagebuilder")
	}
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Fatalf("error parsing log level %q: %v", logLevel, err)
	}
	logrus.SetLevel(level)
	result := m.Run()
	if err = os.RemoveAll(tempdir); err != nil {
		logrus.Errorf("removing temporary directory %q: %v", tempdir, err)
	}
	os.Exit(result)
}

func TestConformance(t *testing.T) {
	dateStamp := fmt.Sprintf("%d", time.Now().UnixNano())
	for i := range internalTestCases {
		t.Run(internalTestCases[i].name, func(t *testing.T) {
			if internalTestCases[i].testUsingSetParent {
				t.Run("new-set-parent", func(t *testing.T) {
					testConformanceInternal(t, dateStamp, i, func(test *testCase) {
						test.dockerBuilderVersion = docker.BuilderBuildKit
						test.compatSetParent = types.OptionalBoolFalse
					})
				})
				t.Run("old-set-parent", func(t *testing.T) {
					testConformanceInternal(t, dateStamp, i, func(test *testCase) {
						test.dockerBuilderVersion = docker.BuilderV1
						test.compatSetParent = types.OptionalBoolTrue
					})
				})
			} else if internalTestCases[i].testUsingVolumes {
				t.Run("new-volumes", func(t *testing.T) {
					testConformanceInternal(t, dateStamp, i, func(test *testCase) {
						test.dockerBuilderVersion = docker.BuilderBuildKit
						test.compatVolumes = types.OptionalBoolFalse
					})
				})
				t.Run("old-volumes", func(t *testing.T) {
					testConformanceInternal(t, dateStamp, i, func(test *testCase) {
						test.dockerBuilderVersion = docker.BuilderV1
						test.compatVolumes = types.OptionalBoolTrue
					})
				})
			} else {
				testConformanceInternal(t, dateStamp, i, nil)
			}
		})
	}
}

func testConformanceInternal(t *testing.T, dateStamp string, testIndex int, mutate func(*testCase)) {
	test := internalTestCases[testIndex]
	if mutate != nil {
		mutate(&test)
	}
	ctx := context.TODO()

	cwd, err := os.Getwd()
	require.NoError(t, err, "error finding current directory")

	// create a temporary directory to hold our build context
	tempdir := t.TempDir()

	// create subdirectories to use as the build context and for buildah storage
	contextDir := filepath.Join(tempdir, "context")
	rootDir := filepath.Join(tempdir, "root")
	runrootDir := filepath.Join(tempdir, "runroot")

	// check if we can test xattrs where we're storing build contexts
	if contextCanDoXattrs == nil {
		testDir := filepath.Join(tempdir, "test")
		if err := os.Mkdir(testDir, 0700); err != nil {
			require.NoErrorf(t, err, "error creating test directory to check if xattrs are testable: %v", err)
		}
		testFile := filepath.Join(testDir, "testfile")
		if err := os.WriteFile(testFile, []byte("whatever"), 0600); err != nil {
			require.NoErrorf(t, err, "error creating test file to check if xattrs are testable: %v", err)
		}
		can := false
		if err := copier.Lsetxattrs(testFile, map[string]string{"user.test": "test"}); err == nil {
			can = true
		}
		contextCanDoXattrs = &can
	}

	// copy either a directory or just a Dockerfile into the temporary directory
	pipeReader, pipeWriter := io.Pipe()
	var getErr, putErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		if test.contextDir != "" {
			getErr = copier.Get("", testDataDir, copier.GetOptions{}, []string{test.contextDir}, pipeWriter)
		} else if test.dockerfile != "" {
			getErr = copier.Get("", testDataDir, copier.GetOptions{}, []string{test.dockerfile}, pipeWriter)
		}
		pipeWriter.Close()
		wg.Done()
	}()
	wg.Add(1)
	go func() {
		if test.contextDir != "" || test.dockerfile != "" {
			putErr = copier.Put("", contextDir, copier.PutOptions{}, pipeReader)
		} else {
			putErr = os.Mkdir(contextDir, 0755)
		}
		pipeReader.Close()
		wg.Done()
	}()
	wg.Wait()
	assert.NoErrorf(t, getErr, "error reading build info from %q", filepath.Join("testdata", test.dockerfile))
	assert.NoErrorf(t, putErr, "error writing build info to %q", contextDir)
	if t.Failed() {
		t.FailNow()
	}

	// construct the names that we want to assign to the images. these should be reasonably unique
	buildahImage := fmt.Sprintf("conformance-buildah:%s-%d", dateStamp, testIndex)
	dockerImage := fmt.Sprintf("conformance-docker:%s-%d", dateStamp, testIndex)
	imagebuilderImage := fmt.Sprintf("conformance-imagebuilder:%s-%d", dateStamp, testIndex)
	if mutate != nil {
		buildahImage += path.Base(t.Name())
		dockerImage += path.Base(t.Name())
		imagebuilderImage += path.Base(t.Name())
	}

	// compute the name of the Dockerfile in the build context directory
	var dockerfileName string
	if test.dockerfile != "" {
		dockerfileName = filepath.Join(contextDir, test.dockerfile)
	} else {
		dockerfileName = filepath.Join(contextDir, "Dockerfile")
	}

	// read the Dockerfile, for inclusion in failure messages
	dockerfileContents := []byte(test.dockerfileContents)
	if len(dockerfileContents) == 0 {
		// no inlined contents -> read them from the specified location
		contents, err := os.ReadFile(dockerfileName)
		require.NoErrorf(t, err, "error reading Dockerfile %q", filepath.Join(tempdir, dockerfileName))
		dockerfileContents = contents
	}

	// initialize storage for buildah
	options := storage.StoreOptions{
		GraphDriverName:     os.Getenv("STORAGE_DRIVER"),
		GraphRoot:           rootDir,
		RunRoot:             runrootDir,
		RootlessStoragePath: rootDir,
	}
	store, err := storage.GetStore(options)
	require.NoErrorf(t, err, "error creating buildah storage at %q", rootDir)
	defer func() {
		if store != nil {
			_, err := store.Shutdown(true)
			require.NoError(t, err, "error shutting down storage for buildah")
		}
	}()
	storageDriver := store.GraphDriverName()
	storageRoot := store.GraphRoot()

	// now that we have a Store, check if we can test xattrs in storage layers
	if storageCanDoXattrs == nil {
		layer, err := store.CreateLayer("", "", nil, "", true, nil)
		if err != nil {
			require.NoErrorf(t, err, "error creating test layer to check if xattrs are testable: %v", err)
		}
		mountPoint, err := store.Mount(layer.ID, "")
		if err != nil {
			require.NoErrorf(t, err, "error mounting test layer to check if xattrs are testable: %v", err)
		}
		testFile := filepath.Join(mountPoint, "testfile")
		if err := os.WriteFile(testFile, []byte("whatever"), 0600); err != nil {
			require.NoErrorf(t, err, "error creating file in test layer to check if xattrs are testable: %v", err)
		}
		can := false
		if err := copier.Lsetxattrs(testFile, map[string]string{"user.test": "test"}); err == nil {
			can = true
		}
		storageCanDoXattrs = &can
		err = store.DeleteLayer(layer.ID)
		if err != nil {
			require.NoErrorf(t, err, "error removing test layer after checking if xattrs are testable: %v", err)
		}
	}

	// connect to dockerd using the docker client library
	dockerClient, err := dockerdockerclient.NewClientWithOpts(dockerdockerclient.FromEnv)
	require.NoError(t, err, "unable to initialize docker.client")
	dockerClient.NegotiateAPIVersion(ctx)
	if test.dockerUseBuildKit || test.dockerBuilderVersion != "" {
		if err := dockerClient.NewVersionError(ctx, "1.38", "buildkit"); err != nil {
			t.Skipf("%v", err)
		}
	}

	// connect to dockerd using go-dockerclient
	client, err := docker.NewClientFromEnv()
	require.NoError(t, err, "unable to initialize docker client")
	var dockerVersion []string
	if version, err := client.Version(); err == nil {
		if version != nil {
			for _, s := range *version {
				dockerVersion = append(dockerVersion, s)
			}
		}
	} else {
		require.NoError(t, err, "unable to connect to docker daemon")
	}

	// make any last-minute tweaks to the build context directory that this test requires
	if test.tweakContextDir != nil {
		err = test.tweakContextDir(t, contextDir, storageDriver, storageRoot)
		require.NoErrorf(t, err, "error tweaking context directory using test-specific callback: %v", err)
	}

	// decide whether we're building just one image for this Dockerfile, or
	// one for each line in it after the first, which we'll assume is a FROM
	if compareLayers {
		// build and compare one line at a time
		line := 1
		for i := range dockerfileContents {
			// scan the byte slice for newlines or the end of the slice, and build using the contents up to that point
			if i == len(dockerfileContents)-1 || (dockerfileContents[i] == '\n' && (i == 0 || dockerfileContents[i-1] != '\\')) {
				if line > 1 || !bytes.HasPrefix(dockerfileContents, []byte("FROM ")) {
					// hack: skip trying to build just the first FROM line
					t.Run(fmt.Sprintf("%d", line), func(t *testing.T) {
						testConformanceInternalBuild(ctx, t, cwd, store, client, dockerClient, fmt.Sprintf("%s.%d", buildahImage, line), fmt.Sprintf("%s.%d", dockerImage, line), fmt.Sprintf("%s.%d", imagebuilderImage, line), contextDir, dockerfileName, dockerfileContents[:i+1], test, line, i == len(dockerfileContents)-1, dockerVersion)
					})
				}
				line++
			}
		}
	} else {
		// build to completion
		testConformanceInternalBuild(ctx, t, cwd, store, client, dockerClient, buildahImage, dockerImage, imagebuilderImage, contextDir, dockerfileName, dockerfileContents, test, 0, true, dockerVersion)
	}
}

func testConformanceInternalBuild(ctx context.Context, t *testing.T, cwd string, store storage.Store, client *docker.Client, dockerClient *dockerdockerclient.Client, buildahImage, dockerImage, imagebuilderImage, contextDir, dockerfileName string, dockerfileContents []byte, test testCase, line int, finalOfSeveral bool, dockerVersion []string) {
	var buildahLog, dockerLog, imagebuilderLog []byte
	var buildahRef, dockerRef, imagebuilderRef types.ImageReference

	// overwrite the Dockerfile in the build context for this run using the
	// contents we were passed, which may only be an initial subset of the
	// original file, or inlined information, in which case the file didn't
	// necessarily exist
	err := os.WriteFile(dockerfileName, dockerfileContents, 0644)
	require.NoErrorf(t, err, "error writing Dockerfile at %q", dockerfileName)
	err = os.Chtimes(dockerfileName, testDate, testDate)
	require.NoErrorf(t, err, "error resetting timestamp on Dockerfile at %q", dockerfileName)
	err = os.Chtimes(contextDir, testDate, testDate)
	require.NoErrorf(t, err, "error resetting timestamp on context directory at %q", contextDir)

	defer func() {
		if t.Failed() {
			if test.contextDir != "" {
				t.Logf("Context %q", filepath.Join(cwd, "testdata", test.contextDir))
			}
			if test.dockerfile != "" {
				if test.contextDir != "" {
					t.Logf("Dockerfile: %q", filepath.Join(cwd, "testdata", test.contextDir, test.dockerfile))
				} else {
					t.Logf("Dockerfile: %q", filepath.Join(cwd, "testdata", test.dockerfile))
				}
			}
			if !bytes.HasSuffix(dockerfileContents, []byte{'\n'}) && !bytes.HasSuffix(dockerfileContents, []byte{'\r'}) {
				dockerfileContents = append(dockerfileContents, []byte("\n(no final end-of-line)")...)
			}
			t.Logf("Dockerfile contents:\n%s", dockerfileContents)
			if dockerignoreContents, err := os.ReadFile(filepath.Join(contextDir, ".dockerignore")); err == nil {
				t.Logf(".dockerignore contents:\n%s", string(dockerignoreContents))
			}
		}
	}()

	// build using docker
	if !test.withoutDocker {
		dockerRef, dockerLog = buildUsingDocker(ctx, t, client, dockerClient, test, dockerImage, contextDir, dockerfileName, line, finalOfSeveral)
		if dockerRef != nil {
			defer func() {
				err := client.RemoveImageExtended(dockerImage, docker.RemoveImageOptions{
					Context: ctx,
					Force:   true,
				})
				assert.Nil(t, err, "error deleting newly-built-by-docker image %q", dockerImage)
			}()
		}
		saveReport(ctx, t, dockerRef, filepath.Join(dockerDir, t.Name()), dockerfileContents, dockerLog, dockerVersion)
		if finalOfSeveral && compareLayers {
			saveReport(ctx, t, dockerRef, filepath.Join(dockerDir, t.Name(), ".."), dockerfileContents, dockerLog, dockerVersion)
		}
	}

	if t.Failed() {
		t.FailNow()
	}

	// build using imagebuilder if we're testing with it, too
	if compareImagebuilder && !test.withoutImagebuilder {
		imagebuilderRef, imagebuilderLog = buildUsingImagebuilder(t, client, test, imagebuilderImage, contextDir, dockerfileName, line, finalOfSeveral)
		if imagebuilderRef != nil {
			defer func() {
				err := client.RemoveImageExtended(imagebuilderImage, docker.RemoveImageOptions{
					Context: ctx,
					Force:   true,
				})
				assert.Nil(t, err, "error deleting newly-built-by-imagebuilder image %q", imagebuilderImage)
			}()
		}
		saveReport(ctx, t, imagebuilderRef, filepath.Join(imagebuilderDir, t.Name()), dockerfileContents, imagebuilderLog, dockerVersion)
		if finalOfSeveral && compareLayers {
			saveReport(ctx, t, imagebuilderRef, filepath.Join(imagebuilderDir, t.Name(), ".."), dockerfileContents, imagebuilderLog, dockerVersion)
		}
	}

	if t.Failed() {
		t.FailNow()
	}

	// always build using buildah
	buildahRef, buildahLog = buildUsingBuildah(ctx, t, store, test, buildahImage, contextDir, dockerfileName, line, finalOfSeveral)
	if buildahRef != nil {
		defer func() {
			err := buildahRef.DeleteImage(ctx, nil)
			assert.Nil(t, err, "error deleting newly-built-by-buildah image %q", buildahImage)
		}()
	}
	saveReport(ctx, t, buildahRef, filepath.Join(buildahDir, t.Name()), dockerfileContents, buildahLog, nil)
	if finalOfSeveral && compareLayers {
		saveReport(ctx, t, buildahRef, filepath.Join(buildahDir, t.Name(), ".."), dockerfileContents, buildahLog, nil)
	}

	if t.Failed() {
		t.FailNow()
	}

	if test.shouldFailAt != 0 {
		// the build is expected to fail, so there's no point in comparing information about any images
		return
	}

	// the report on the buildah image should always be there
	_, originalBuildahConfig, ociBuildahConfig, fsBuildah := readReport(t, filepath.Join(buildahDir, t.Name()))
	if t.Failed() {
		t.FailNow()
	}
	deleteLabel := func(config map[string]interface{}, label string) {
		for _, configName := range []string{"config", "container_config"} {
			if configStruct, ok := config[configName]; ok {
				if configMap, ok := configStruct.(map[string]interface{}); ok {
					if labels, ok := configMap["Labels"]; ok {
						if labelMap, ok := labels.(map[string]interface{}); ok {
							delete(labelMap, label)
						}
					}
				}
			}
		}
	}
	deleteLabel(originalBuildahConfig, buildah.BuilderIdentityAnnotation)
	deleteLabel(ociBuildahConfig, buildah.BuilderIdentityAnnotation)

	var originalDockerConfig, ociDockerConfig, fsDocker map[string]interface{}

	// the report on the docker image should be there if we expected the build to succeed
	if !test.withoutDocker {
		var mediaType string
		mediaType, originalDockerConfig, ociDockerConfig, fsDocker = readReport(t, filepath.Join(dockerDir, t.Name()))
		assert.Equal(t, manifest.DockerV2Schema2MediaType, mediaType, "Image built by docker build didn't use Docker MIME type - tests require update")
		if t.Failed() {
			t.FailNow()
		}

		miss, left, diff, same := compareJSON(originalDockerConfig, originalBuildahConfig, originalSkip)
		if !same {
			assert.Failf(t, "Image configurations differ as committed in Docker format", configCompareResult(miss, left, diff, "buildah"))
		}
		miss, left, diff, same = compareJSON(ociDockerConfig, ociBuildahConfig, ociSkip)
		if !same {
			assert.Failf(t, "Image configurations differ when converted to OCI format", configCompareResult(miss, left, diff, "buildah"))
		}
		miss, left, diff, same = compareJSON(fsDocker, fsBuildah, append(fsSkip, test.fsSkip...))
		if !same {
			assert.Failf(t, "Filesystem contents differ", fsCompareResult(miss, left, diff, "buildah"))
		}
	}

	// the report on the imagebuilder image should be there if we expected the build to succeed
	if compareImagebuilder && !test.withoutImagebuilder {
		_, originalDockerConfig, ociDockerConfig, fsDocker = readReport(t, filepath.Join(dockerDir, t.Name()))
		if t.Failed() {
			t.FailNow()
		}

		_, originalImagebuilderConfig, ociImagebuilderConfig, fsImagebuilder := readReport(t, filepath.Join(imagebuilderDir, t.Name()))
		if t.Failed() {
			t.FailNow()
		}

		// compare the reports between docker and imagebuilder
		miss, left, diff, same := compareJSON(originalDockerConfig, originalImagebuilderConfig, originalSkip)
		if !same {
			assert.Failf(t, "Image configurations differ as committed in Docker format", configCompareResult(miss, left, diff, "imagebuilder"))
		}
		miss, left, diff, same = compareJSON(ociDockerConfig, ociImagebuilderConfig, ociSkip)
		if !same {
			assert.Failf(t, "Image configurations differ when converted to OCI format", configCompareResult(miss, left, diff, "imagebuilder"))
		}
		miss, left, diff, same = compareJSON(fsDocker, fsImagebuilder, append(fsSkip, test.fsSkip...))
		if !same {
			assert.Failf(t, "Filesystem contents differ", fsCompareResult(miss, left, diff, "imagebuilder"))
		}
	}
}

func buildUsingBuildah(ctx context.Context, t *testing.T, store storage.Store, test testCase, buildahImage, contextDir, dockerfileName string, line int, finalOfSeveral bool) (buildahRef types.ImageReference, buildahLog []byte) {
	// buildah tests might be using transient mounts. replace "@@TEMPDIR@@"
	// in such specifications with the path of the context directory
	var transientMounts []string
	for _, mount := range test.transientMounts {
		transientMounts = append(transientMounts, strings.Replace(mount, "@@TEMPDIR@@", contextDir, 1))
	}
	// set up build options
	output := &bytes.Buffer{}
	if test.compatSetParent != types.OptionalBoolUndefined {
		compat := "default"
		switch test.compatSetParent {
		case types.OptionalBoolFalse:
			compat = "false"
		case types.OptionalBoolTrue:
			compat = "true"
		}
		t.Logf("using buildah flag CompatSetParent = %s", compat)
	}
	if test.compatVolumes != types.OptionalBoolUndefined {
		compat := "default"
		switch test.compatVolumes {
		case types.OptionalBoolFalse:
			compat = "false"
		case types.OptionalBoolTrue:
			compat = "true"
		}
		t.Logf("using buildah flag CompatVolumes = %s", compat)
	}
	options := define.BuildOptions{
		ContextDirectory: contextDir,
		CommonBuildOpts:  &define.CommonBuildOptions{},
		NamespaceOptions: []define.NamespaceOption{{
			Name: string(rspec.NetworkNamespace),
			Host: true,
		}},
		TransientMounts:         transientMounts,
		Output:                  buildahImage,
		OutputFormat:            buildah.Dockerv2ImageManifest,
		Out:                     output,
		Err:                     output,
		Layers:                  true,
		NoCache:                 true,
		RemoveIntermediateCtrs:  true,
		ForceRmIntermediateCtrs: true,
		CompatSetParent:         test.compatSetParent,
		CompatVolumes:           test.compatVolumes,
	}
	// build the image and gather output. log the output if the build part of the test failed
	imageID, _, err := imagebuildah.BuildDockerfiles(ctx, store, options, dockerfileName)
	if err != nil {
		output.WriteString("\n" + err.Error())
	}

	outputString := output.String()
	defer func() {
		if t.Failed() {
			t.Logf("buildah output:\n%s", outputString)
		}
	}()

	buildPost(t, test, err, "buildah", outputString, test.buildahRegex, test.buildahErrRegex, line, finalOfSeveral)

	// return a reference to the new image, if we succeeded
	if err == nil {
		buildahRef, err = istorage.Transport.ParseStoreReference(store, imageID)
		assert.Nil(t, err, "error parsing reference to newly-built image with ID %q", imageID)
	}
	return buildahRef, []byte(outputString)
}

func pullImageIfMissing(t *testing.T, client *docker.Client, image string) {
	if _, err := client.InspectImage(image); err != nil {
		repository, tag := docker.ParseRepositoryTag(image)
		if tag == "" {
			tag = "latest"
		}
		pullOptions := docker.PullImageOptions{
			Repository: repository,
			Tag:        tag,
		}
		pullAuths := docker.AuthConfiguration{}
		if err := client.PullImage(pullOptions, pullAuths); err != nil {
			t.Fatalf("while pulling %q: %v", image, err)
		}
	}
}

func buildUsingDocker(ctx context.Context, t *testing.T, client *docker.Client, dockerClient *dockerdockerclient.Client, test testCase, dockerImage, contextDir, dockerfileName string, line int, finalOfSeveral bool) (dockerRef types.ImageReference, dockerLog []byte) {
	// compute the path of the dockerfile relative to the build context
	dockerfileRelativePath, err := filepath.Rel(contextDir, dockerfileName)
	require.NoErrorf(t, err, "unable to compute path of dockerfile %q relative to context directory %q", dockerfileName, contextDir)

	// read the Dockerfile so that we can pull base images
	dockerfileContent, err := os.ReadFile(dockerfileName)
	require.NoErrorf(t, err, "reading dockerfile %q", dockerfileName)
	for _, line := range strings.Split(string(dockerfileContent), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# syntax=") {
			pullImageIfMissing(t, client, strings.TrimPrefix(line, "# syntax="))
		}
	}
	parsed, err := imagebuilder.ParseDockerfile(bytes.NewReader(dockerfileContent))
	require.NoErrorf(t, err, "parsing dockerfile %q", dockerfileName)
	dummyBuilder := imagebuilder.NewBuilder(nil)
	stages, err := imagebuilder.NewStages(parsed, dummyBuilder)
	require.NoErrorf(t, err, "breaking dockerfile %q up into stages", dockerfileName)
	for i := range stages {
		stageBase, err := dummyBuilder.From(stages[i].Node)
		require.NoErrorf(t, err, "parsing base image for stage %d in %q", i, dockerfileName)
		if stageBase == "" || stageBase == imagebuilder.NoBaseImageSpecifier {
			continue
		}
		needToEnsureBase := true
		for j := 0; j < i; j++ {
			if stageBase == stages[j].Name {
				needToEnsureBase = false
			}
		}
		if !needToEnsureBase {
			continue
		}
		pullImageIfMissing(t, client, stageBase)
	}

	excludes, err := imagebuilder.ParseDockerignore(contextDir)
	require.NoErrorf(t, err, "parsing ignores file in %q", contextDir)
	excludes = append(excludes, "!"+dockerfileRelativePath, "!.dockerignore")
	tarOptions := &archive.TarOptions{
		ExcludePatterns: excludes,
		ChownOpts:       &idtools.IDPair{UID: 0, GID: 0},
	}
	input, err := archive.TarWithOptions(contextDir, tarOptions)
	require.NoErrorf(t, err, "archiving context directory %q", contextDir)
	defer input.Close()

	// set up build options
	output := &bytes.Buffer{}
	options := docker.BuildImageOptions{
		Context:             ctx,
		Dockerfile:          dockerfileRelativePath,
		InputStream:         input,
		OutputStream:        output,
		Name:                dockerImage,
		NoCache:             true,
		RmTmpContainer:      true,
		ForceRmTmpContainer: true,
	}
	if test.dockerUseBuildKit || test.dockerBuilderVersion != "" {
		if test.dockerBuilderVersion != "" {
			var version string
			switch test.dockerBuilderVersion {
			case docker.BuilderBuildKit:
				version = "BuildKit"
			case docker.BuilderV1:
				version = "V1 (classic)"
			default:
				version = "(unknown)"
			}
			t.Logf("requesting docker builder %s", version)
			options.Version = test.dockerBuilderVersion
		} else {
			t.Log("requesting docker builder BuildKit")
			options.Version = docker.BuilderBuildKit
		}
	}
	// build the image and gather output. log the output if the build part of the test failed
	err = client.BuildImage(options)
	if err != nil {
		output.WriteString("\n" + err.Error())
	}
	if _, err := dockerClient.BuildCachePrune(ctx, dockertypes.BuildCachePruneOptions{All: true}); err != nil {
		t.Logf("docker build cache prune: %v", err)
	}

	outputString := output.String()
	defer func() {
		if t.Failed() {
			t.Logf("docker build output:\n%s", outputString)
		}
	}()

	buildPost(t, test, err, "docker build", outputString, test.dockerRegex, test.dockerErrRegex, line, finalOfSeveral)

	// return a reference to the new image, if we succeeded
	if err == nil {
		dockerRef, err = daemon.ParseReference(dockerImage)
		assert.Nil(t, err, "error parsing reference to newly-built image with name %q", dockerImage)
	}
	return dockerRef, []byte(outputString)
}

func buildUsingImagebuilder(t *testing.T, client *docker.Client, test testCase, imagebuilderImage, contextDir, dockerfileName string, line int, finalOfSeveral bool) (imagebuilderRef types.ImageReference, imagebuilderLog []byte) {
	// compute the path of the dockerfile relative to the build context
	dockerfileRelativePath, err := filepath.Rel(contextDir, dockerfileName)
	require.NoErrorf(t, err, "unable to compute path of dockerfile %q relative to context directory %q", dockerfileName, contextDir)
	// set up build options
	output := &bytes.Buffer{}
	executor := dockerclient.NewClientExecutor(client)
	executor.Directory = contextDir
	executor.Tag = imagebuilderImage
	executor.AllowPull = true
	executor.Out = output
	executor.ErrOut = output
	executor.LogFn = func(format string, args ...interface{}) {
		fmt.Fprintf(output, "--> %s\n", fmt.Sprintf(format, args...))
	}
	// buildah tests might be using transient mounts. replace "@@TEMPDIR@@"
	// in such specifications with the path of the context directory
	for _, mount := range test.transientMounts {
		var src, dest string
		mountSpec := strings.SplitN(strings.Replace(mount, "@@TEMPDIR@@", contextDir, 1), ":", 2)
		if len(mountSpec) > 1 {
			src = mountSpec[0]
		}
		dest = mountSpec[len(mountSpec)-1]
		executor.TransientMounts = append(executor.TransientMounts, dockerclient.Mount{
			SourcePath:      src,
			DestinationPath: dest,
		})
	}
	// build the image and gather output. log the output if the build part of the test failed
	builder := imagebuilder.NewBuilder(nil)
	node, err := imagebuilder.ParseFile(filepath.Join(contextDir, dockerfileRelativePath))
	if err != nil {
		assert.Nil(t, err, "error parsing Dockerfile: %v", err)
	}
	if _, err = os.Stat(filepath.Join(contextDir, ".dockerignore")); err == nil {
		if builder.Excludes, err = imagebuilder.ParseDockerignore(contextDir); err != nil {
			assert.Nil(t, err, "error parsing .dockerignore file: %v", err)
		}
	}
	stages, err := imagebuilder.NewStages(node, builder)
	if err != nil {
		assert.Nil(t, err, "error breaking Dockerfile into stages")
	} else {
		if finalExecutor, err := executor.Stages(builder, stages, ""); err != nil {
			output.WriteString("\n" + err.Error())
		} else {
			if err = finalExecutor.Commit(stages[len(stages)-1].Builder); err != nil {
				assert.Nil(t, err, "error committing final stage: %v", err)
			}
		}
	}

	outputString := output.String()
	defer func() {
		if t.Failed() {
			t.Logf("imagebuilder build output:\n%s", outputString)
		}
		for err := range executor.Release() {
			t.Logf("imagebuilder build post-error: %v", err)
		}
	}()

	buildPost(t, test, err, "imagebuilder", outputString, test.imagebuilderRegex, test.imagebuilderErrRegex, line, finalOfSeveral)

	// return a reference to the new image, if we succeeded
	if err == nil {
		imagebuilderRef, err = daemon.ParseReference(imagebuilderImage)
		assert.Nil(t, err, "error parsing reference to newly-built image with name %q", imagebuilderImage)
	}
	return imagebuilderRef, []byte(outputString)
}

func buildPost(t *testing.T, test testCase, err error, buildTool, outputString, stdoutRegex, stderrRegex string, line int, finalOfSeveral bool) {
	// check if the build succeeded or failed, whichever was expected
	if test.shouldFailAt != 0 && (line == 0 || line >= test.shouldFailAt) {
		// this is expected to fail, and we're either at/past
		// the line where it should fail, or we're not going
		// line-by-line
		assert.NotNil(t, err, fmt.Sprintf("%s build was expected to fail, but succeeded", buildTool))
	} else {
		assert.Nil(t, err, fmt.Sprintf("%s build was expected to succeed, but failed", buildTool))
	}

	// if the build failed, and we have an error message we expected, check for it
	if err != nil && test.failureRegex != "" {
		outputTokens := strings.Join(strings.Fields(err.Error()), " ")
		assert.Regexpf(t, regexp.MustCompile(test.failureRegex), outputTokens, "build failure did not match %q", test.failureRegex)
	}

	// if this is the last image we're building for this case, we can scan
	// the build log for expected messages
	if finalOfSeveral {
		outputTokens := strings.Join(strings.Fields(outputString), " ")
		// check for expected output
		if stdoutRegex != "" {
			assert.Regexpf(t, regexp.MustCompile(stdoutRegex), outputTokens, "build output did not match %q", stdoutRegex)
		}
		if stderrRegex != "" {
			assert.Regexpf(t, regexp.MustCompile(stderrRegex), outputTokens, "build error did not match %q", stderrRegex)
		}
	}
}

// FSTree holds the information we have about an image's filesystem
type FSTree struct {
	Layers []Layer `json:"layers,omitempty"`
	Tree   FSEntry `json:"tree,omitempty"`
}

// Layer keeps track of the digests and contents of a layer blob
type Layer struct {
	UncompressedDigest digest.Digest `json:"uncompressed-digest,omitempty"`
	CompressedDigest   digest.Digest `json:"compressed-digest,omitempty"`
	Headers            []FSHeader    `json:"-,omitempty"`
}

// FSHeader is the parts of the tar.Header for an entry in a layer blob that
// are relevant
type FSHeader struct {
	Typeflag byte              `json:"typeflag,omitempty"`
	Name     string            `json:"name,omitempty"`
	Linkname string            `json:"linkname,omitempty"`
	Size     int64             `json:"size"`
	Mode     int64             `json:"mode,omitempty"`
	UID      int               `json:"uid"`
	GID      int               `json:"gid"`
	ModTime  time.Time         `json:"mtime,omitempty"`
	Devmajor int64             `json:"devmajor,omitempty"`
	Devminor int64             `json:"devminor,omitempty"`
	Xattrs   map[string]string `json:"xattrs,omitempty"`
	Digest   digest.Digest     `json:"digest,omitempty"`
}

// FSEntry stores one item in a filesystem tree.  If it represents a directory,
// its contents are stored as its children
type FSEntry struct {
	FSHeader
	Children map[string]*FSEntry `json:"(dir),omitempty"`
}

// fsHeaderForEntry converts a tar header to an FSHeader, in the process
// discarding some fields which we don't care to compare
func fsHeaderForEntry(hdr *tar.Header) FSHeader {
	return FSHeader{
		Typeflag: hdr.Typeflag,
		Name:     hdr.Name,
		Linkname: hdr.Linkname,
		Size:     hdr.Size,
		Mode:     (hdr.Mode & int64(fs.ModePerm)),
		UID:      hdr.Uid,
		GID:      hdr.Gid,
		ModTime:  hdr.ModTime,
		Devmajor: hdr.Devmajor,
		Devminor: hdr.Devminor,
		Xattrs:   hdr.Xattrs, // nolint:staticcheck
	}
}

// save information about the specified image to the specified directory
func saveReport(ctx context.Context, t *testing.T, ref types.ImageReference, directory string, dockerfileContents []byte, buildLog []byte, version []string) {
	imageName := ""
	// make sure the directory exists
	err := os.MkdirAll(directory, 0755)
	require.NoErrorf(t, err, "error ensuring directory %q exists for storing a report", directory)
	// save the Dockerfile that was used to generate the image
	err = os.WriteFile(filepath.Join(directory, "Dockerfile"), dockerfileContents, 0644)
	require.NoErrorf(t, err, "error saving Dockerfile for image %q", imageName)
	// save the log generated while building the image
	err = os.WriteFile(filepath.Join(directory, "build.log"), buildLog, 0644)
	require.NoErrorf(t, err, "error saving build log for image %q", imageName)
	// save the version information
	if len(version) > 0 {
		err = os.WriteFile(filepath.Join(directory, "version"), []byte(strings.Join(version, "\n")+"\n"), 0644)
		require.NoErrorf(t, err, "error saving builder version information for image %q", imageName)
	}
	// open the image for reading
	if ref == nil {
		return
	}
	imageName = transports.ImageName(ref)
	src, err := ref.NewImageSource(ctx, nil)
	require.NoErrorf(t, err, "error opening image %q as source to read its configuration", imageName)
	closer := io.Closer(src)
	defer func() {
		closer.Close()
	}()
	img, err := image.FromSource(ctx, nil, src)
	require.NoErrorf(t, err, "error opening image %q to read its configuration", imageName)
	closer = img
	// read the manifest in its original form
	rawManifest, _, err := src.GetManifest(ctx, nil)
	require.NoErrorf(t, err, "error reading raw manifest from image %q", imageName)
	// read the config blob in its original form
	rawConfig, err := img.ConfigBlob(ctx)
	require.NoErrorf(t, err, "error reading configuration from image %q", imageName)
	// read the config blob, converted to OCI format by the image library, and re-encode it
	ociConfig, err := img.OCIConfig(ctx)
	require.NoErrorf(t, err, "error reading OCI-format configuration from image %q", imageName)
	encodedConfig, err := json.Marshal(ociConfig)
	require.NoErrorf(t, err, "error encoding OCI-format configuration from image %q for saving", imageName)
	// save the manifest in its original form
	err = os.WriteFile(filepath.Join(directory, "manifest.json"), rawManifest, 0644)
	require.NoErrorf(t, err, "error saving original manifest from image %q", imageName)
	// save the config blob in the OCI format
	err = os.WriteFile(filepath.Join(directory, "oci-config.json"), encodedConfig, 0644)
	require.NoErrorf(t, err, "error saving OCI-format configuration from image %q", imageName)
	// save the config blob in its original format
	err = os.WriteFile(filepath.Join(directory, "config.json"), rawConfig, 0644)
	require.NoErrorf(t, err, "error saving original configuration from image %q", imageName)
	// start pulling layer information
	layerBlobInfos, err := img.LayerInfosForCopy(ctx)
	require.NoErrorf(t, err, "error reading blob infos for image %q", imageName)
	if len(layerBlobInfos) == 0 {
		layerBlobInfos = img.LayerInfos()
	}
	fstree := FSTree{Tree: FSEntry{Children: make(map[string]*FSEntry)}}
	// grab digest and header information from the layer blob
	for _, layerBlobInfo := range layerBlobInfos {
		rc, _, err := src.GetBlob(ctx, layerBlobInfo, nil)
		require.NoErrorf(t, err, "error reading blob %+v for image %q", layerBlobInfo, imageName)
		defer rc.Close()
		layer := summarizeLayer(t, imageName, layerBlobInfo, rc)
		fstree.Layers = append(fstree.Layers, layer)
	}
	// apply the header information from blobs, in the order they're listed
	// in the config blob, to produce what we think the filesystem tree
	// would look like
	for _, diffID := range ociConfig.RootFS.DiffIDs {
		var layer *Layer
		for i := range fstree.Layers {
			if fstree.Layers[i].CompressedDigest == diffID {
				layer = &fstree.Layers[i]
				break
			}
			if fstree.Layers[i].UncompressedDigest == diffID {
				layer = &fstree.Layers[i]
				break
			}
		}
		if layer == nil {
			require.Failf(t, "missing layer diff", "config for image %q specifies a layer with diffID %q, but we found no layer blob matching that digest", imageName, diffID)
		}
		applyLayerToFSTree(t, layer, &fstree.Tree)
	}
	// encode the filesystem tree information and save it to a file,
	// discarding the layer summaries because different tools may choose
	// between marking a directory as opaque and removing each of its
	// contents individually, which would produce the same result, so
	// there's no point in saving them for comparison later
	encodedFSTree, err := json.Marshal(fstree.Tree)
	require.NoErrorf(t, err, "error encoding filesystem tree from image %q for saving", imageName)
	err = os.WriteFile(filepath.Join(directory, "fs.json"), encodedFSTree, 0644)
	require.NoErrorf(t, err, "error saving filesystem tree from image %q", imageName)
}

// summarizeLayer reads a blob and returns a summary of the parts of its contents that we care about
func summarizeLayer(t *testing.T, imageName string, blobInfo types.BlobInfo, reader io.Reader) (layer Layer) {
	compressedDigest := digest.Canonical.Digester()
	counter := ioutils.NewWriteCounter(compressedDigest.Hash())
	compressionAlgorithm, _, reader, err := compression.DetectCompressionFormat(reader)
	require.NoErrorf(t, err, "error checking if blob %+v for image %q is compressed", blobInfo, imageName)
	uncompressedBlob, wasCompressed, err := compression.AutoDecompress(io.TeeReader(reader, counter))
	require.NoErrorf(t, err, "error decompressing blob %+v for image %q", blobInfo, imageName)
	defer uncompressedBlob.Close()
	uncompressedDigest := digest.Canonical.Digester()
	tarToRead := io.TeeReader(uncompressedBlob, uncompressedDigest.Hash())
	tr := tar.NewReader(tarToRead)
	hdr, err := tr.Next()
	for err == nil {
		header := fsHeaderForEntry(hdr)
		if hdr.Size != 0 {
			contentDigest := digest.Canonical.Digester()
			n, err := io.Copy(contentDigest.Hash(), tr)
			require.NoErrorf(t, err, "error digesting contents of %q from layer %+v for image %q", hdr.Name, blobInfo, imageName)
			require.Equal(t, hdr.Size, n, "error reading contents of %q from layer %+v for image %q: wrong size", hdr.Name, blobInfo, imageName)
			header.Digest = contentDigest.Digest()
		}
		layer.Headers = append(layer.Headers, header)
		hdr, err = tr.Next()
	}
	require.Equal(t, io.EOF, err, "unexpected error reading layer contents %+v for image %q", blobInfo, imageName)
	_, err = io.Copy(io.Discard, tarToRead)
	require.NoError(t, err, "reading out any not-usually-present zero padding at the end")
	layer.CompressedDigest = compressedDigest.Digest()
	blobFormatDescription := "uncompressed"
	if wasCompressed {
		if compressionAlgorithm.Name() != "" {
			blobFormatDescription = "compressed with " + compressionAlgorithm.Name()
		} else {
			blobFormatDescription = "compressed (?)"
		}
	}
	require.Equalf(t, blobInfo.Digest, layer.CompressedDigest, "calculated digest of %s blob didn't match expected digest (expected length %d, actual length %d)", blobFormatDescription, blobInfo.Size, counter.Count)
	layer.UncompressedDigest = uncompressedDigest.Digest()
	return layer
}

// applyLayerToFSTree updates the in-memory summary of a tree to incorporate
// changes described in the layer.  This is a little naive, in that we don't
// expect the pathname to include symlinks, which we don't resolve, as
// components, but tools that currently generate layer diffs don't create
// those.
func applyLayerToFSTree(t *testing.T, layer *Layer, root *FSEntry) {
	for i, entry := range layer.Headers {
		if entry.Typeflag == tar.TypeLink {
			// if the entry is a hard link, replace it with the
			// contents of the hard-linked file
			replaced := false
			name := entry.Name
			for j, otherEntry := range layer.Headers {
				if j >= i {
					break
				}
				if otherEntry.Name == entry.Linkname {
					entry = otherEntry
					entry.Name = name
					replaced = true
					break
				}
			}
			if !replaced {
				require.Fail(t, "layer diff error", "hardlink entry referenced a file that isn't in the layer")
			}
		}
		// parse the name from the entry, and don't get tripped up by a final '/'
		dirEntry := root
		components := strings.Split(strings.Trim(entry.Name, string(os.PathSeparator)), string(os.PathSeparator))
		require.NotEmpty(t, entry.Name, "layer diff error", "entry has no name")
		require.NotZerof(t, len(components), "entry name %q has no components", entry.Name)
		require.NotZerof(t, components[0], "entry name %q has no components", entry.Name)
		// "split" the final part of the path from the rest
		base := components[len(components)-1]
		components = components[:len(components)-1]
		// find the directory that contains this entry
		for i, component := range components {
			// this should be a parent directory, so check if it looks like a parent directory
			if dirEntry.Children == nil {
				require.Failf(t, "layer diff error", "layer diff %q includes entry for %q, but %q is not a directory", layer.UncompressedDigest, entry.Name, strings.Join(components[:i], string(os.PathSeparator)))
			}
			// if the directory is already there, move into it
			if child, ok := dirEntry.Children[component]; ok {
				dirEntry = child
				continue
			}
			// if the directory should be there, but we haven't
			// created it yet, blame the tool that generated this
			// layer diff
			require.Failf(t, "layer diff error", "layer diff %q includes entry for %q, but %q doesn't exist", layer.UncompressedDigest, entry.Name, strings.Join(components[:i], string(os.PathSeparator)))
		}
		// if the current directory is marked as "opaque", remove all
		// of its contents
		if base == ".wh..opq" {
			dirEntry.Children = make(map[string]*FSEntry)
			continue
		}
		// if the item is a whiteout, strip the "this is a whiteout
		// entry" prefix and remove the item it names
		if strings.HasPrefix(base, ".wh.") {
			delete(dirEntry.Children, strings.TrimPrefix(base, ".wh."))
			continue
		}
		// if the item already exists, make sure we don't get confused
		// by replacing a directory with a non-directory or vice-versa
		if child, ok := dirEntry.Children[base]; ok {
			if child.Children != nil {
				// it's a directory
				if entry.Typeflag == tar.TypeDir {
					// new entry is a directory, too. no
					// sweat, just update the metadata
					child.FSHeader = entry
					continue
				}
				// nope, a directory no longer
			} else {
				// it's not a directory
				if entry.Typeflag != tar.TypeDir {
					// new entry is not a directory, too.
					// no sweat, just update the metadata
					dirEntry.Children[base].FSHeader = entry
					continue
				}
				// well, it's a directory now
			}
		}
		// the item doesn't already exist, or it needs to be replaced, so we need to add it
		var children map[string]*FSEntry
		if entry.Typeflag == tar.TypeDir {
			// only directory entries can hold items
			children = make(map[string]*FSEntry)
		}
		dirEntry.Children[base] = &FSEntry{FSHeader: entry, Children: children}
	}
}

// read information about the specified image from the specified directory
func readReport(t *testing.T, directory string) (manifestType string, original, oci, fs map[string]interface{}) {
	// read the manifest in the as-committed format, whatever that is
	originalManifest, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	require.NoErrorf(t, err, "error reading manifest %q", filepath.Join(directory, "manifest.json"))
	// dump it into a map
	manifest := make(map[string]interface{})
	err = json.Unmarshal(originalManifest, &manifest)
	require.NoErrorf(t, err, "error decoding manifest %q", filepath.Join(directory, "manifest.json"))
	if str, ok := manifest["mediaType"].(string); ok {
		manifestType = str
	}
	// read the config in the as-committed (docker) format
	originalConfig, err := os.ReadFile(filepath.Join(directory, "config.json"))
	require.NoErrorf(t, err, "error reading configuration file %q", filepath.Join(directory, "config.json"))
	// dump it into a map
	original = make(map[string]interface{})
	err = json.Unmarshal(originalConfig, &original)
	require.NoErrorf(t, err, "error decoding configuration from file %q", filepath.Join(directory, "config.json"))
	// read the config in converted-to-OCI format
	ociConfig, err := os.ReadFile(filepath.Join(directory, "oci-config.json"))
	require.NoErrorf(t, err, "error reading OCI configuration file %q", filepath.Join(directory, "oci-config.json"))
	// dump it into a map
	oci = make(map[string]interface{})
	err = json.Unmarshal(ociConfig, &oci)
	require.NoErrorf(t, err, "error decoding OCI configuration from file %q", filepath.Join(directory, "oci.json"))
	// read the filesystem
	fsInfo, err := os.ReadFile(filepath.Join(directory, "fs.json"))
	require.NoErrorf(t, err, "error reading filesystem summary file %q", filepath.Join(directory, "fs.json"))
	// dump it into a map for comparison
	fs = make(map[string]interface{})
	err = json.Unmarshal(fsInfo, &fs)
	require.NoErrorf(t, err, "error decoding filesystem summary from file %q", filepath.Join(directory, "fs.json"))
	// return both
	return manifestType, original, oci, fs
}

// contains is used to check if item exists in []string or not, ignoring case
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if strings.EqualFold(s, item) {
			return true
		}
	}
	return false
}

// addPrefix prepends the given prefix to each string in []string.
// The prefix and the string are joined with ":"
func addPrefix(a []string, prefix string) []string {
	b := make([]string, 0, len(a))
	for _, s := range a {
		b = append(b, prefix+":"+s)
	}
	return b
}

// diffDebug returns a row for a tabwriter that summarizes a field name and the
// values for that field in two documents
func diffDebug(k string, a, b interface{}) string {
	if k == "mode" {
		// force modes to be displayed in octal instead of decimal
		a, aok := a.(float64)
		b, bok := b.(float64)
		if aok && bok {
			return fmt.Sprintf("%v\t0%o\t0%o\n", k, int64(a), int64(b))
		}
	}
	return fmt.Sprintf("%v\t%v\t%v\n", k, a, b)
}

// compareJSON compares two parsed JSON structures.  missKeys and leftKeys are
// lists of field names present only in the first map or the second,
// respectively, while diffKeys is a list of items which are present in both
// maps, but which have different values, formatted with diffDebug.
func compareJSON(a, b map[string]interface{}, skip []string) (missKeys, leftKeys, diffKeys []string, isSame bool) {
	isSame = true

	for k, v := range a {
		vb, ok := b[k]
		if ok {
			// remove this item from b. when we're done, all that's
			// left in b will be the items that weren't also in a.
			delete(b, k)
		}
		if contains(skip, k) {
			continue
		}
		if !ok {
			// key is in a, but not in b
			missKeys = append(missKeys, k)
			isSame = false
			continue
		}
		if reflect.TypeOf(v) != reflect.TypeOf(vb) {
			if reflect.TypeOf(v) == nil && reflect.ValueOf(vb).Len() == 0 {
				continue
			}
			if reflect.TypeOf(vb) == nil && reflect.ValueOf(v).Len() == 0 {
				continue
			}
			diffKeys = append(diffKeys, diffDebug(k, v, vb))
			isSame = false
			continue
		}
		switch v.(type) {
		case map[string]interface{}:
			// this field in the object is itself an object (e.g.
			// "config" or "container_config"), so recursively
			// compare them
			var nextSkip []string
			prefix := k + ":"
			for _, s := range skip {
				if strings.HasPrefix(s, prefix) {
					nextSkip = append(nextSkip, strings.TrimPrefix(s, prefix))
				}
			}
			submiss, subleft, subdiff, ok := compareJSON(v.(map[string]interface{}), vb.(map[string]interface{}), nextSkip)
			missKeys = append(missKeys, addPrefix(submiss, k)...)
			leftKeys = append(leftKeys, addPrefix(subleft, k)...)
			diffKeys = append(diffKeys, addPrefix(subdiff, k)...)
			if !ok {
				isSame = false
			}
		case []interface{}:
			// this field in the object is an array; make sure both
			// arrays have the same set of elements, which is more
			// or less correct for labels and environment
			// variables.
			// this will break if it tries to compare an array of
			// objects like "history", since maps, slices, and
			// functions can't be used as keys in maps
			tmpa := v.([]interface{})
			tmpb := vb.([]interface{})
			if len(tmpa) != len(tmpb) {
				diffKeys = append(diffKeys, diffDebug(k, v, vb))
				isSame = false
				break
			}
			m := make(map[interface{}]struct{})
			for i := 0; i < len(tmpb); i++ {
				m[tmpb[i]] = struct{}{}
			}
			for i := 0; i < len(tmpa); i++ {
				if _, ok := m[tmpa[i]]; !ok {
					diffKeys = append(diffKeys, diffDebug(k, v, vb))
					isSame = false
					break
				}
			}
		default:
			// this field in the object is neither an object nor an
			// array, so assume it's a scalar item
			if !reflect.DeepEqual(v, vb) {
				diffKeys = append(diffKeys, diffDebug(k, v, vb))
				isSame = false
			}
		}
	}

	if len(b) > 0 {
		for k := range b {
			if !contains(skip, k) {
				leftKeys = append(leftKeys, k)
			}
		}
	}

	return slices.Clone(missKeys), slices.Clone(leftKeys), slices.Clone(diffKeys), isSame
}

// configCompareResult summarizes the output of compareJSON for display
func configCompareResult(miss, left, diff []string, notDocker string) string {
	var buffer bytes.Buffer
	if len(miss) > 0 {
		buffer.WriteString(fmt.Sprintf("Fields missing from %s version: %s\n", notDocker, strings.Join(miss, " ")))
	}
	if len(left) > 0 {
		buffer.WriteString(fmt.Sprintf("Fields which only exist in %s version: %s\n", notDocker, strings.Join(left, " ")))
	}
	if len(diff) > 0 {
		buffer.WriteString("Fields present in both versions have different values:\n")
		tw := tabwriter.NewWriter(&buffer, 1, 1, 8, ' ', 0)
		if _, err := tw.Write([]byte(fmt.Sprintf("Field\tDocker\t%s\n", notDocker))); err != nil {
			panic(err)
		}
		for _, d := range diff {
			if _, err := tw.Write([]byte(d)); err != nil {
				panic(err)
			}
		}
		tw.Flush()
	}
	return buffer.String()
}

// fsCompareResult summarizes the output of compareJSON for display
func fsCompareResult(miss, left, diff []string, notDocker string) string {
	var buffer bytes.Buffer
	fixup := func(names []string) []string {
		n := make([]string, 0, len(names))
		for _, name := range names {
			n = append(n, strings.ReplaceAll(strings.ReplaceAll(name, ":(dir):", "/"), "(dir):", "/"))
		}
		return n
	}
	if len(miss) > 0 {
		buffer.WriteString(fmt.Sprintf("Content missing from %s version: %s\n", notDocker, strings.Join(fixup(miss), " ")))
	}
	if len(left) > 0 {
		buffer.WriteString(fmt.Sprintf("Content which only exists in %s version: %s\n", notDocker, strings.Join(fixup(left), " ")))
	}
	if len(diff) > 0 {
		buffer.WriteString("File attributes in both versions have different values:\n")
		tw := tabwriter.NewWriter(&buffer, 1, 1, 8, ' ', 0)
		if _, err := tw.Write([]byte(fmt.Sprintf("File:attr\tDocker\t%s\n", notDocker))); err != nil {
			panic(err)
		}
		for _, d := range fixup(diff) {
			if _, err := tw.Write([]byte(d)); err != nil {
				panic(err)
			}
		}
		tw.Flush()
	}
	return buffer.String()
}

type testCaseTweakContextDirFn func(*testing.T, string, string, string) error
type testCase struct {
	name                 string                    // name of the test
	dockerfileContents   string                    // inlined Dockerfile content to use instead of possible file in the build context
	dockerfile           string                    // name of the Dockerfile, relative to contextDir, if not Dockerfile
	contextDir           string                    // name of context subdirectory, if there is one to be copied
	tweakContextDir      testCaseTweakContextDirFn // callback to make updates to the temporary build context before we build it
	shouldFailAt         int                       // line where a build is expected to fail (starts with 1, 0 if it should succeed
	buildahRegex         string                    // if set, expect this to be present in output
	dockerRegex          string                    // if set, expect this to be present in output
	imagebuilderRegex    string                    // if set, expect this to be present in output
	buildahErrRegex      string                    // if set, expect this to be present in output
	dockerErrRegex       string                    // if set, expect this to be present in output
	imagebuilderErrRegex string                    // if set, expect this to be present in output
	failureRegex         string                    // if set, expect this to be present in output when the build fails
	withoutImagebuilder  bool                      // don't build this with imagebuilder, because it depends on a buildah-specific feature
	withoutDocker        bool                      // don't build this with docker, because it depends on a buildah-specific feature
	dockerUseBuildKit    bool                      // if building with docker, request that dockerd use buildkit
	dockerBuilderVersion docker.BuilderVersion     // if building with docker, request the specific builder
	testUsingSetParent   bool                      // test both with old (gets set) and new (left blank) config.Parent behavior
	compatSetParent      types.OptionalBool        // placeholder for a value to set for the buildah compatSetParent flag
	testUsingVolumes     bool                      // test both with old (preserved) and new (just a config note) volume behavior
	compatVolumes        types.OptionalBool        // placeholder for a value to set for the buildah compatVolumes flag
	transientMounts      []string                  // one possible buildah-specific feature
	fsSkip               []string                  // expected filesystem differences, typically timestamps on files or directories we create or modify during the build and don't reset
}

var internalTestCases = []testCase{
	{
		name:         "shell test",
		dockerfile:   "Dockerfile.shell",
		buildahRegex: "(?s)[0-9a-z]+(.*)--",
		dockerRegex:  "(?s)RUN env.*?Running in [0-9a-z]+(.*?)---",
	},

	{
		name:         "copy file to root",
		dockerfile:   "Dockerfile.copyfrom_1",
		buildahRegex: "[-rw]+.*?/a",
		fsSkip:       []string{"(dir):a:mtime"},
	},

	{
		name:         "copy file to same file",
		dockerfile:   "Dockerfile.copyfrom_2",
		buildahRegex: "[-rw]+.*?/a",
		fsSkip:       []string{"(dir):a:mtime"},
	},

	{
		name:         "copy file to workdir",
		dockerfile:   "Dockerfile.copyfrom_3",
		buildahRegex: "[-rw]+.*?/b/a",
		fsSkip:       []string{"(dir):b:mtime", "(dir):b:(dir):a:mtime"},
	},

	{
		name:         "copy file to workdir rename",
		dockerfile:   "Dockerfile.copyfrom_3_1",
		buildahRegex: "[-rw]+.*?/b/b",
		fsSkip:       []string{"(dir):b:mtime", "(dir):b:(dir):a:mtime"},
	},

	{
		name:            "copy folder contents to higher level",
		dockerfile:      "Dockerfile.copyfrom_4",
		buildahRegex:    "(?s)[-rw]+.*?/b/1.*?[-rw]+.*?/b/2.*?/b.*?[-rw]+.*?1.*?[-rw]+.*?2",
		buildahErrRegex: "/a: No such file or directory",
		fsSkip:          []string{"(dir):b:mtime"},
	},

	{
		name:            "copy wildcard folder contents to higher level",
		dockerfile:      "Dockerfile.copyfrom_5",
		buildahRegex:    "(?s)[-rw]+.*?/b/1.*?[-rw]+.*?/b/2.*?/b.*?[-rw]+.*?1.*?[-rw]+.*?2",
		buildahErrRegex: "(?s)/a: No such file or directory.*?/b/a: No such file or directory.*?/b/b: No such file or director",
		fsSkip:          []string{"(dir):b:mtime", "(dir):b:(dir):1:mtime", "(dir):b:(dir):2:mtime"},
	},

	{
		name:            "copy folder with dot contents to higher level",
		dockerfile:      "Dockerfile.copyfrom_6",
		buildahRegex:    "(?s)[-rw]+.*?/b/1.*?[-rw]+.*?/b/2.*?/b.*?[-rw]+.*?1.*?[-rw]+.*?2",
		buildahErrRegex: "(?s)/a: No such file or directory.*?/b/a: No such file or directory.*?/b/b: No such file or director",
		fsSkip:          []string{"(dir):b:mtime", "(dir):b:(dir):1:mtime", "(dir):b:(dir):2:mtime"},
	},

	{
		name:            "copy root file to different root name",
		dockerfile:      "Dockerfile.copyfrom_7",
		buildahRegex:    "[-rw]+.*?/a",
		buildahErrRegex: "/b: No such file or directory",
		fsSkip:          []string{"(dir):a:mtime"},
	},

	{
		name:            "copy nested file to different root name",
		dockerfile:      "Dockerfile.copyfrom_8",
		buildahRegex:    "[-rw]+.*?/a",
		buildahErrRegex: "/b: No such file or directory",
		fsSkip:          []string{"(dir):a:mtime"},
	},

	{
		name:            "copy file to deeper directory with explicit slash",
		dockerfile:      "Dockerfile.copyfrom_9",
		buildahRegex:    "[-rw]+.*?/a/b/c/1",
		buildahErrRegex: "/a/b/1: No such file or directory",
		fsSkip:          []string{"(dir):a:mtime", "(dir):a:(dir):b:mtime", "(dir):a:(dir):b:(dir):c:mtime", "(dir):a:(dir):b:(dir):c:(dir):1:mtime"},
	},

	{
		name:            "copy file to deeper directory without explicit slash",
		dockerfile:      "Dockerfile.copyfrom_10",
		buildahRegex:    "[-rw]+.*?/a/b/c",
		buildahErrRegex: "/a/b/1: No such file or directory",
		fsSkip:          []string{"(dir):a:mtime", "(dir):a:(dir):b:mtime", "(dir):a:(dir):b:(dir):c:mtime"},
	},

	{
		name:            "copy directory to deeper directory without explicit slash",
		dockerfile:      "Dockerfile.copyfrom_11",
		buildahRegex:    "[-rw]+.*?/a/b/c/1",
		buildahErrRegex: "/a/b/1: No such file or directory",
		fsSkip: []string{
			"(dir):a:mtime", "(dir):a:(dir):b:mtime", "(dir):a:(dir):b:(dir):c:mtime",
			"(dir):a:(dir):b:(dir):c:(dir):1:mtime",
		},
	},

	{
		name:            "copy directory to root without explicit slash",
		dockerfile:      "Dockerfile.copyfrom_12",
		buildahRegex:    "[-rw]+.*?/a/1",
		buildahErrRegex: "/a/a: No such file or directory",
		fsSkip:          []string{"(dir):a:mtime", "(dir):a:(dir):1:mtime"},
	},

	{
		name:            "copy directory trailing to root without explicit slash",
		dockerfile:      "Dockerfile.copyfrom_13",
		buildahRegex:    "[-rw]+.*?/a/1",
		buildahErrRegex: "/a/a: No such file or directory",
		fsSkip:          []string{"(dir):a:mtime", "(dir):a:(dir):1:mtime"},
	},

	{
		name:         "multi stage base",
		dockerfile:   "Dockerfile.reusebase",
		buildahRegex: "[0-9a-z]+ /1",
		fsSkip:       []string{"(dir):1:mtime"},
	},

	{
		name:       "directory",
		contextDir: "dir",
		fsSkip:     []string{"(dir):dir:mtime", "(dir):test:mtime"},
	},

	{
		name:       "copy to dir",
		contextDir: "copy",
		fsSkip:     []string{"(dir):usr:(dir):bin:mtime"},
	},

	{
		name:       "copy dir",
		contextDir: "copydir",
		fsSkip:     []string{"(dir):dir"},
	},

	{
		name:       "copy from symlink source",
		contextDir: "copysymlink",
	},

	{
		name:       "copy-symlink-2",
		contextDir: "copysymlink",
		dockerfile: "Dockerfile2",
	},

	{
		name:       "copy from subdir to new directory",
		contextDir: "copydir",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY dir/file /subdir/",
		}, "\n"),
		fsSkip: []string{"(dir):subdir"},
	},

	{
		name:       "copy to renamed file",
		contextDir: "copyrename",
		fsSkip:     []string{"(dir):usr:(dir):bin:mtime"},
	},

	{
		name:       "copy with --chown",
		contextDir: "copychown",
		fsSkip:     []string{"(dir):usr:(dir):bin:mtime", "(dir):usr:(dir):local:(dir):bin:mtime"},
	},

	{
		name:       "directory with slash",
		contextDir: "overlapdirwithslash",
	},

	{
		name:       "directory without slash",
		contextDir: "overlapdirwithoutslash",
	},

	{
		name:         "environment",
		dockerfile:   "Dockerfile.env",
		shouldFailAt: 12,
	},

	{
		name:       "edgecases",
		dockerfile: "Dockerfile.edgecases",
		fsSkip: []string{
			"(dir):test:mtime", "(dir):test:(dir):copy:mtime", "(dir):test2:mtime", "(dir):test3:mtime",
			"(dir):test3:(dir):copy:mtime",
			"(dir):test3:(dir):test:mtime", "(dir):tmp:mtime", "(dir):tmp:(dir):passwd:mtime",
		},
	},

	{
		name:               "exposed default",
		dockerfile:         "Dockerfile.exposedefault",
		testUsingSetParent: true,
	},

	{
		name:       "add",
		dockerfile: "Dockerfile.add",
		fsSkip:     []string{"(dir):b:mtime", "(dir):tmp:mtime"},
	},

	{
		name:         "run with JSON",
		dockerfile:   "Dockerfile.run.args",
		buildahRegex: "(first|third|fifth|inner) (second|fourth|sixth|outer)",
		dockerRegex:  "Running in [0-9a-z]+.*?(first|third|fifth|inner) (second|fourth|sixth|outer)",
	},

	{
		name:       "wildcard",
		contextDir: "wildcard",
		fsSkip:     []string{"(dir):usr:mtime", "(dir):usr:(dir):test:mtime"},
	},

	{
		name:             "volume",
		contextDir:       "volume",
		fsSkip:           []string{"(dir):var:mtime", "(dir):var:(dir):www:mtime"},
		testUsingVolumes: true,
	},

	{
		name:             "volumerun",
		contextDir:       "volumerun",
		fsSkip:           []string{"(dir):var:mtime", "(dir):var:(dir):www:mtime"},
		testUsingVolumes: true,
	},

	{
		name:            "mount",
		contextDir:      "mount",
		buildahRegex:    "/tmp/test/file.*?regular file.*?/tmp/test/file2.*?regular file",
		withoutDocker:   true,
		transientMounts: []string{"@@TEMPDIR@@:/tmp/test" + selinuxMountFlag()},
	},

	{
		name:          "transient-mount",
		contextDir:    "transientmount",
		buildahRegex:  "file2.*?FROM busybox ENV name value",
		withoutDocker: true,
		transientMounts: []string{
			"@@TEMPDIR@@:/mountdir" + selinuxMountFlag(),
			"@@TEMPDIR@@/Dockerfile.env:/mountfile" + selinuxMountFlag(),
		},
	},

	{
		// from internal team chat
		name: "ci-pipeline-modified",
		dockerfileContents: strings.Join([]string{
			"FROM busybox",
			"WORKDIR /go/src/github.com/openshift/ocp-release-operator-sdk/",
			"ENV GOPATH=/go",
			"RUN env | grep -E -v '^(HOSTNAME|OLDPWD)=' | LANG=C sort | tee /env-contents.txt\n",
		}, "\n"),
		fsSkip: []string{
			"(dir):go:mtime",
			"(dir):go:(dir):src:mtime",
			"(dir):go:(dir):src:(dir):github.com:mtime",
			"(dir):go:(dir):src:(dir):github.com:(dir):openshift:mtime",
			"(dir):go:(dir):src:(dir):github.com:(dir):openshift:(dir):ocp-release-operator-sdk:mtime",
			"(dir):env-contents.txt:mtime",
		},
	},

	{
		name:          "add-permissions",
		withoutDocker: true,
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"# Does ADD preserve permissions differently for archives and files?",
			"ADD archive.tar subdir1/",
			"ADD archive/ subdir2/",
		}, "\n"),
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			content := []byte("test content")

			if err := os.Mkdir(filepath.Join(contextDir, "archive"), 0755); err != nil {
				return fmt.Errorf("creating subdirectory of temporary context directory: %w", err)
			}
			filename := filepath.Join(contextDir, "archive", "should-be-owned-by-root")
			if err = os.WriteFile(filename, content, 0640); err != nil {
				return fmt.Errorf("creating file owned by root in temporary context directory: %w", err)
			}
			if err = os.Chown(filename, 0, 0); err != nil {
				return fmt.Errorf("setting ownership on file owned by root in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on file owned by root file in temporary context directory: %w", err)
			}
			filename = filepath.Join(contextDir, "archive", "should-be-owned-by-99")
			if err = os.WriteFile(filename, content, 0640); err != nil {
				return fmt.Errorf("creating file owned by 99 in temporary context directory: %w", err)
			}
			if err = os.Chown(filename, 99, 99); err != nil {
				return fmt.Errorf("setting ownership on file owned by 99 in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on file owned by 99 in temporary context directory: %w", err)
			}

			filename = filepath.Join(contextDir, "archive.tar")
			f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("creating archive file: %w", err)
			}
			defer f.Close()
			tw := tar.NewWriter(f)
			defer tw.Close()
			err = tw.WriteHeader(&tar.Header{
				Name:     "archive/should-be-owned-by-root",
				Typeflag: tar.TypeReg,
				Size:     int64(len(content)),
				ModTime:  testDate,
				Mode:     0640,
				Uid:      0,
				Gid:      0,
			})
			if err != nil {
				return fmt.Errorf("writing archive file header: %w", err)
			}
			n, err := tw.Write(content)
			if err != nil {
				return fmt.Errorf("writing archive file contents: %w", err)
			}
			if n != len(content) {
				return errors.New("short write writing archive file contents")
			}
			err = tw.WriteHeader(&tar.Header{
				Name:     "archive/should-be-owned-by-99",
				Typeflag: tar.TypeReg,
				Size:     int64(len(content)),
				ModTime:  testDate,
				Mode:     0640,
				Uid:      99,
				Gid:      99,
			})
			if err != nil {
				return fmt.Errorf("writing archive file header: %w", err)
			}
			n, err = tw.Write(content)
			if err != nil {
				return fmt.Errorf("writing archive file contents: %w", err)
			}
			if n != len(content) {
				return errors.New("short write writing archive file contents")
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir1:mtime", "(dir):subdir2:mtime"},
	},

	{
		name: "copy-permissions",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"# Does COPY --chown change permissions on already-present directories?",
			"COPY subdir/ subdir/",
			"COPY --chown=99:99 subdir/ subdir/",
		}, "\n"),
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			content := []byte("test content")

			if err := os.Mkdir(filepath.Join(contextDir, "subdir"), 0755); err != nil {
				return fmt.Errorf("creating subdirectory of temporary context directory: %w", err)
			}
			filename := filepath.Join(contextDir, "subdir", "would-be-owned-by-root")
			if err = os.WriteFile(filename, content, 0640); err != nil {
				return fmt.Errorf("creating file owned by root in temporary context directory: %w", err)
			}
			if err = os.Chown(filename, 0, 0); err != nil {
				return fmt.Errorf("setting ownership on file owned by root in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on file owned by root file in temporary context directory: %w", err)
			}
			filename = filepath.Join(contextDir, "subdir", "would-be-owned-by-99")
			if err = os.WriteFile(filename, content, 0640); err != nil {
				return fmt.Errorf("creating file owned by 99 in temporary context directory: %w", err)
			}
			if err = os.Chown(filename, 99, 99); err != nil {
				return fmt.Errorf("setting ownership on file owned by 99 in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on file owned by 99 in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "copy-permissions-implicit",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"# Does COPY --chown change permissions on already-present directories?",
			"COPY --chown=99:99 subdir/ subdir/",
			"COPY subdir/ subdir/",
		}, "\n"),
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			content := []byte("test content")

			if err := os.Mkdir(filepath.Join(contextDir, "subdir"), 0755); err != nil {
				return fmt.Errorf("creating subdirectory of temporary context directory: %w", err)
			}
			filename := filepath.Join(contextDir, "subdir", "would-be-owned-by-root")
			if err = os.WriteFile(filename, content, 0640); err != nil {
				return fmt.Errorf("creating file owned by root in temporary context directory: %w", err)
			}
			if err = os.Chown(filename, 0, 0); err != nil {
				return fmt.Errorf("setting ownership on file owned by root in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on file owned by root file in temporary context directory: %w", err)
			}
			filename = filepath.Join(contextDir, "subdir", "would-be-owned-by-99")
			if err = os.WriteFile(filename, content, 0640); err != nil {
				return fmt.Errorf("creating file owned by 99 in temporary context directory: %w", err)
			}
			if err = os.Chown(filename, 99, 99); err != nil {
				return fmt.Errorf("setting ownership on file owned by 99 in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on file owned by 99 in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		// the digest just ensures that we can handle a digest
		// reference to a manifest list; the digest of any manifest
		// list in the image repository would do
		name: "stage-container-as-source-plus-hardlinks",
		dockerfileContents: strings.Join([]string{
			"FROM busybox@sha256:9ddee63a712cea977267342e8750ecbc60d3aab25f04ceacfa795e6fce341793 AS build",
			"RUN mkdir -p /target/subdir",
			"RUN cp -p /etc/passwd /target/",
			"RUN ln /target/passwd /target/subdir/passwd",
			"RUN ln /target/subdir/passwd /target/subdir/passwd2",
			"FROM scratch",
			"COPY --from=build /target/subdir /subdir",
		}, "\n"),
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name:       "dockerfile-in-subdirectory",
		dockerfile: "subdir/Dockerfile",
		contextDir: "subdir",
	},

	{
		name: "setuid-file-in-context",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			fmt.Sprintf("# Does the setuid file (0%o) in the context dir end up setuid in the image?", syscall.S_ISUID),
			"COPY . subdir1",
			"ADD  . subdir2",
		}, "\n"),
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			filename := filepath.Join(contextDir, "should-be-setuid-file")
			if err = os.WriteFile(filename, []byte("test content"), 0644); err != nil {
				return fmt.Errorf("creating setuid test file in temporary context directory: %w", err)
			}
			if err = syscall.Chmod(filename, syscall.S_ISUID|0755); err != nil {
				return fmt.Errorf("setting setuid bit on test file in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on setuid test file in temporary context directory: %w", err)
			}
			filename = filepath.Join(contextDir, "should-be-setgid-file")
			if err = os.WriteFile(filename, []byte("test content"), 0644); err != nil {
				return fmt.Errorf("creating setgid test file in temporary context directory: %w", err)
			}
			if err = syscall.Chmod(filename, syscall.S_ISGID|0755); err != nil {
				return fmt.Errorf("setting setgid bit on test file in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on setgid test file in temporary context directory: %w", err)
			}
			filename = filepath.Join(contextDir, "should-be-sticky-file")
			if err = os.WriteFile(filename, []byte("test content"), 0644); err != nil {
				return fmt.Errorf("creating sticky test file in temporary context directory: %w", err)
			}
			if err = syscall.Chmod(filename, syscall.S_ISVTX|0755); err != nil {
				return fmt.Errorf("setting permissions on sticky test file in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on sticky test file in temporary context directory: %w", err)
			}
			filename = filepath.Join(contextDir, "should-not-be-setuid-setgid-sticky-file")
			if err = os.WriteFile(filename, []byte("test content"), 0644); err != nil {
				return fmt.Errorf("creating non-suid non-sgid non-sticky test file in temporary context directory: %w", err)
			}
			if err = syscall.Chmod(filename, 0640); err != nil {
				return fmt.Errorf("setting permissions on plain test file in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on plain test file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir1:mtime", "(dir):subdir2:mtime"},
	},

	{
		name: "xattrs-file-in-context",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"# Do the xattrs on a file in the context dir end up in the image?",
			"COPY . subdir1",
			"ADD  . subdir2",
		}, "\n"),
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			if !*contextCanDoXattrs {
				t.Skipf("test context directory %q doesn't support xattrs", contextDir)
			}
			if !*storageCanDoXattrs {
				t.Skipf("test storage driver %q and directory %q don't support xattrs together", storageDriver, storageRoot)
			}

			filename := filepath.Join(contextDir, "xattrs-file")
			if err = os.WriteFile(filename, []byte("test content"), 0644); err != nil {
				return fmt.Errorf("creating test file with xattrs in temporary context directory: %w", err)
			}
			if err = copier.Lsetxattrs(filename, map[string]string{"user.a": "test"}); err != nil {
				return fmt.Errorf("setting xattrs on test file in temporary context directory: %w", err)
			}
			if err = syscall.Chmod(filename, 0640); err != nil {
				return fmt.Errorf("setting permissions on test file in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on test file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir1:mtime", "(dir):subdir2:mtime"},
	},

	{
		name: "setuid-file-in-archive",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			fmt.Sprintf("# Do the setuid/setgid/sticky files in this archive end up setuid(0%o)/setgid(0%o)/sticky(0%o)?", syscall.S_ISUID, syscall.S_ISGID, syscall.S_ISVTX),
			"ADD archive.tar .",
		}, "\n"),
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			filename := filepath.Join(contextDir, "archive.tar")
			f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("creating new archive file in temporary context directory: %w", err)
			}
			defer f.Close()
			tw := tar.NewWriter(f)
			defer tw.Close()
			hdr := tar.Header{
				Name:     "setuid-file",
				Uid:      0,
				Gid:      0,
				Typeflag: tar.TypeReg,
				Size:     8,
				Mode:     cISUID | 0755,
				ModTime:  testDate,
			}
			if err = tw.WriteHeader(&hdr); err != nil {
				return fmt.Errorf("writing tar archive header: %w", err)
			}
			if _, err = io.Copy(tw, bytes.NewReader([]byte("whatever"))); err != nil {
				return fmt.Errorf("writing tar archive content: %w", err)
			}
			hdr = tar.Header{
				Name:     "setgid-file",
				Uid:      0,
				Gid:      0,
				Typeflag: tar.TypeReg,
				Size:     8,
				Mode:     cISGID | 0755,
				ModTime:  testDate,
			}
			if err = tw.WriteHeader(&hdr); err != nil {
				return fmt.Errorf("writing tar archive header: %w", err)
			}
			if _, err = io.Copy(tw, bytes.NewReader([]byte("whatever"))); err != nil {
				return fmt.Errorf("writing tar archive content: %w", err)
			}
			hdr = tar.Header{
				Name:     "sticky-file",
				Uid:      0,
				Gid:      0,
				Typeflag: tar.TypeReg,
				Size:     8,
				Mode:     cISVTX | 0755,
				ModTime:  testDate,
			}
			if err = tw.WriteHeader(&hdr); err != nil {
				return fmt.Errorf("writing tar archive header: %w", err)
			}
			if _, err = io.Copy(tw, bytes.NewReader([]byte("whatever"))); err != nil {
				return fmt.Errorf("writing tar archive content: %w", err)
			}
			hdr = tar.Header{
				Name:     "setuid-dir",
				Uid:      0,
				Gid:      0,
				Typeflag: tar.TypeDir,
				Size:     0,
				Mode:     cISUID | 0755,
				ModTime:  testDate,
			}
			if err = tw.WriteHeader(&hdr); err != nil {
				return fmt.Errorf("error writing tar archive header: %w", err)
			}
			hdr = tar.Header{
				Name:     "setgid-dir",
				Uid:      0,
				Gid:      0,
				Typeflag: tar.TypeDir,
				Size:     0,
				Mode:     cISGID | 0755,
				ModTime:  testDate,
			}
			if err = tw.WriteHeader(&hdr); err != nil {
				return fmt.Errorf("error writing tar archive header: %w", err)
			}
			hdr = tar.Header{
				Name:     "sticky-dir",
				Uid:      0,
				Gid:      0,
				Typeflag: tar.TypeDir,
				Size:     0,
				Mode:     cISVTX | 0755,
				ModTime:  testDate,
			}
			if err = tw.WriteHeader(&hdr); err != nil {
				return fmt.Errorf("error writing tar archive header: %w", err)
			}
			return nil
		},
	},

	{
		name: "xattrs-file-in-archive",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"# Do the xattrs on a file in an archive end up in the image?",
			"ADD archive.tar .",
		}, "\n"),
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			if !*contextCanDoXattrs {
				t.Skipf("test context directory %q doesn't support xattrs", contextDir)
			}
			if !*storageCanDoXattrs {
				t.Skipf("test storage driver %q and directory %q don't support xattrs together", storageDriver, storageRoot)
			}

			filename := filepath.Join(contextDir, "archive.tar")
			f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("creating new archive file in temporary context directory: %w", err)
			}
			defer f.Close()
			tw := tar.NewWriter(f)
			defer tw.Close()
			hdr := tar.Header{
				Name:     "xattr-file",
				Uid:      0,
				Gid:      0,
				Typeflag: tar.TypeReg,
				Size:     8,
				Mode:     0640,
				ModTime:  testDate,
				Xattrs:   map[string]string{"user.a": "test"},
			}
			if err = tw.WriteHeader(&hdr); err != nil {
				return fmt.Errorf("writing tar archive header: %w", err)
			}
			if _, err = io.Copy(tw, bytes.NewReader([]byte("whatever"))); err != nil {
				return fmt.Errorf("writing tar archive content: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir1:mtime", "(dir):subdir2:mtime"},
	},

	{
		// docker build apparently stopped preserving this bit somewhere between 18.09.7 and 19.03,
		// possibly around https://github.com/moby/moby/pull/38599
		name: "setuid-file-in-other-stage",
		dockerfileContents: strings.Join([]string{
			"FROM busybox",
			"RUN mkdir /a && echo whatever > /a/setuid && chmod u+xs /a/setuid && touch -t @1485449953 /a/setuid",
			"RUN mkdir /b && echo whatever > /b/setgid && chmod g+xs /b/setgid && touch -t @1485449953 /b/setgid",
			"RUN mkdir /c && echo whatever > /c/sticky && chmod o+x /c/sticky && chmod +t /c/sticky && touch -t @1485449953 /c/sticky",
			"FROM scratch",
			fmt.Sprintf("# Does this setuid/setgid/sticky file copied from another stage end up setuid/setgid/sticky (0%o/0%o/0%o)?", syscall.S_ISUID, syscall.S_ISGID, syscall.S_ISVTX),
			"COPY --from=0 /a/setuid /b/setgid /c/sticky /",
		}, "\n"),
	},

	{
		name: "xattrs-file-in-other-stage",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . .",
			"FROM scratch",
			"# Do the xattrs on a file in another stage end up in the image?",
			"COPY --from=0 /xattrs-file /",
		}, "\n"),
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			if !*contextCanDoXattrs {
				t.Skipf("test context directory %q doesn't support xattrs", contextDir)
			}
			if !*storageCanDoXattrs {
				t.Skipf("test storage driver %q and directory %q don't support xattrs together", storageDriver, storageRoot)
			}

			filename := filepath.Join(contextDir, "xattrs-file")
			if err = os.WriteFile(filename, []byte("test content"), 0644); err != nil {
				return fmt.Errorf("creating test file with xattrs in temporary context directory: %w", err)
			}
			if err = copier.Lsetxattrs(filename, map[string]string{"user.a": "test"}); err != nil {
				return fmt.Errorf("setting xattrs on test file in temporary context directory: %w", err)
			}
			if err = syscall.Chmod(filename, 0640); err != nil {
				return fmt.Errorf("setting permissions on test file in temporary context directory: %w", err)
			}
			if err = os.Chtimes(filename, testDate, testDate); err != nil {
				return fmt.Errorf("setting date on test file in temporary context directory: %w", err)
			}
			return nil
		},
	},

	{
		name: "copy-multiple-some-missing",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY file-a.txt subdir-a file-z.txt subdir-z subdir/",
		}, "\n"),
		contextDir:   "dockerignore/populated",
		shouldFailAt: 2,
	},

	{
		name: "copy-multiple-missing-file-with-glob",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY file-z.txt subdir-* subdir/",
		}, "\n"),
		contextDir:   "dockerignore/populated",
		shouldFailAt: 2,
	},

	{
		name: "copy-multiple-missing-file-with-nomatch-on-glob",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY missing* subdir/",
		}, "\n"),
		contextDir:   "dockerignore/populated",
		shouldFailAt: 2,
	},

	{
		name: "copy-multiple-some-missing-glob",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY file-a.txt subdir-* file-?.txt missing* subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		fsSkip:     []string{"(dir):subdir:mtime"},
	},

	{
		name: "file-in-workdir-in-other-stage",
		dockerfileContents: strings.Join([]string{
			"FROM scratch AS base",
			"COPY . /subdir/",
			"WORKDIR /subdir",
			"FROM base",
			"COPY --from=base . .", // --from=otherstage ignores that stage's WORKDIR
		}, "\n"),
		contextDir: "dockerignore/populated",
		fsSkip:     []string{"(dir):subdir:mtime", "(dir):subdir:(dir):subdir:mtime"},
	},

	{
		name:         "copy-integration1",
		contextDir:   "dockerignore/integration1",
		shouldFailAt: 3,
		failureRegex: "(no such file or directory)|(file not found)|(file does not exist)",
	},

	{
		name:       "copy-integration2",
		contextDir: "dockerignore/integration2",
	},

	{
		name:         "copy-integration3",
		contextDir:   "dockerignore/integration3",
		shouldFailAt: 4,
		failureRegex: "(no such file or directory)|(file not found)|(file does not exist)",
	},

	{
		name:       "copy-empty-1",
		contextDir: "copyempty",
		dockerfile: "Dockerfile",
		fsSkip:     []string{"(dir):usr:(dir):local:mtime", "(dir):usr:(dir):local:(dir):tmp:mtime"},
	},

	{
		name:       "copy-empty-2",
		contextDir: "copyempty",
		dockerfile: "Dockerfile2",
		fsSkip:     []string{"(dir):usr:(dir):local:mtime", "(dir):usr:(dir):local:(dir):tmp:mtime"},
	},

	{
		name:       "copy-absolute-directory-1",
		contextDir: "copyblahblub",
		dockerfile: "Dockerfile",
		fsSkip:     []string{"(dir):var:mtime"},
	},

	{
		name:       "copy-absolute-directory-2",
		contextDir: "copyblahblub",
		dockerfile: "Dockerfile2",
		fsSkip:     []string{"(dir):var:mtime"},
	},

	{
		name:       "copy-absolute-directory-3",
		contextDir: "copyblahblub",
		dockerfile: "Dockerfile3",
		fsSkip:     []string{"(dir):var:mtime"},
	},

	{
		name: "multi-stage-through-base",
		dockerfileContents: strings.Join([]string{
			"FROM alpine AS base",
			"RUN touch -t @1485449953 /1",
			"ENV LOCAL=/1",
			"RUN find $LOCAL",
			"FROM base",
			"RUN find $LOCAL",
		}, "\n"),
		fsSkip: []string{"(dir):root:mtime", "(dir):1:mtime"},
	},

	{
		name: "multi-stage-derived", // from #2415
		dockerfileContents: strings.Join([]string{
			"FROM busybox as layer",
			"RUN touch /root/layer",
			"FROM layer as derived",
			"RUN touch -t @1485449953 /root/derived ; rm /root/layer",
			"FROM busybox AS output",
			"COPY --from=layer /root /root",
		}, "\n"),
		fsSkip: []string{"(dir):root:mtime", "(dir):root:(dir):layer:mtime"},
	},

	{
		name:          "dockerignore-minimal-test", // from #2237
		contextDir:    "dockerignore/minimal_test",
		withoutDocker: true,
		fsSkip:        []string{"(dir):tmp:mtime", "(dir):tmp:(dir):stuff:mtime"},
	},

	{
		name:       "dockerignore-is-even-there",
		contextDir: "dockerignore/empty",
		fsSkip:     []string{"(dir):subdir:mtime"},
	},

	{
		name:       "dockerignore-irrelevant",
		contextDir: "dockerignore/empty",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte(strings.Join([]string{"**/*-a", "!**/*-c"}, "\n"))
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0600); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exceptions-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte(strings.Join([]string{"**/*-a", "!**/*-c"}, "\n"))
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0644); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-extensions-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte(strings.Join([]string{"**/*-a", "!**/*-c"}, "\n"))
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0600); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-includes-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte(strings.Join([]string{"!**/*-c"}, "\n"))
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0640); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-includes-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("!**/*-c\n")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0100); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-plain-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("subdir-c")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0200); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-plain-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("subdir-c")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0400); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-plain-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("**/subdir-c")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0200); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-plain-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("**/subdir-c")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0400); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-wildcard-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("subdir-*")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0000); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-wildcard-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("subdir-*")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0660); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-deep-wildcard-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("**/subdir-*")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0000); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-deep-wildcard-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("**/subdir-*")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0660); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-deep-subdir-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("**/subdir-f")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0666); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-deep-subdir-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("**/subdir-f")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0640); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-not-so-deep-subdir-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("**/subdir-b")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0705); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-not-so-deep-subdir-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte("**/subdir-b")
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0750); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-kind-of-deep-subdir-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte(strings.Join([]string{"**/subdir-e", "!**/subdir-f"}, "\n"))
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0750); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-kind-of-deep-subdir-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte(strings.Join([]string{"**/subdir-e", "!**/subdir-f"}, "\n"))
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0750); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-deep-subdir-dot",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY . subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte(strings.Join([]string{"**/subdir-f", "!**/subdir-g"}, "\n"))
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0750); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name: "dockerignore-exclude-deep-subdir-star",
		dockerfileContents: strings.Join([]string{
			"FROM scratch",
			"COPY * subdir/",
		}, "\n"),
		contextDir: "dockerignore/populated",
		tweakContextDir: func(t *testing.T, contextDir, storageDriver, storageRoot string) (err error) {
			dockerignore := []byte(strings.Join([]string{"**/subdir-f", "!**/subdir-g"}, "\n"))
			if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), dockerignore, 0750); err != nil {
				return fmt.Errorf("writing .dockerignore file: %w", err)
			}
			if err = os.Chtimes(filepath.Join(contextDir, ".dockerignore"), testDate, testDate); err != nil {
				return fmt.Errorf("setting date on .dockerignore file in temporary context directory: %w", err)
			}
			return nil
		},
		fsSkip: []string{"(dir):subdir:mtime"},
	},

	{
		name:       "env-whitespace",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name value`,
		}, "\n"),
	},

	{
		name:       "env-simple",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name=value`,
		}, "\n"),
	},

	{
		name:       "env-unquoted-list",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name=value name2=value2`,
		}, "\n"),
	},

	{
		name:       "env-dquoted-list",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name="value value1"`,
		}, "\n"),
	},

	{
		name:       "env-escaped-value",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name=value\ value2`,
		}, "\n"),
	},

	{
		name:       "env-squote-in-dquote",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name="value'quote space'value2"`,
		}, "\n"),
	},

	{
		name:       "env-dquote-in-squote",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name='value"double quote"value2'`,
		}, "\n"),
	},

	{
		name:       "env-escaped-list",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name=value\ value2 name2=value2\ value3`,
		}, "\n"),
	},

	{
		name:       "env-eddquote",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name="a\"b"`,
		}, "\n"),
	},

	{
		name:       "env-invalid-ssquote",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name='a\'b'`,
		}, "\n"),
		shouldFailAt: 3,
	},

	{
		name:       "env-esdquote",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name="a\'b"`,
		}, "\n"),
	},

	{
		name:       "env-essquote",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name='a\'b''`,
		}, "\n"),
	},

	{
		name:       "env-edsquote",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name='a\"b'`,
		}, "\n"),
	},

	{
		name:       "env-empty-squote-in-empty-dquote",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`ENV name="''"`,
		}, "\n"),
	},

	{
		name:       "env-multiline",
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM scratch`,
			`COPY script .`,
			`# don't put anything after the next line - it must be the last line of the`,
			`# Dockerfile and it must end with \`,
			`ENV name=value \`,
			`    name1=value1 \`,
			`    name2="value2a \`,
			`           value2b" \`,
			`    name3="value3a\n\"value3b\"" \`,
			`        name4="value4a\\nvalue4b" \`,
		}, "\n"),
	},

	{
		name: "copy-from-owner", // from issue #2518
		dockerfileContents: strings.Join([]string{
			`FROM alpine`,
			`RUN set -ex; touch -t @1485449953 /test; chown 65:65 /test`,
			`FROM scratch`,
			`USER 66:66`,
			`COPY --from=0 /test /test`,
		}, "\n"),
		fsSkip: []string{"test:mtime"},
	},

	{
		name: "copy-from-owner-with-chown", // issue #2518, but with chown to override
		dockerfileContents: strings.Join([]string{
			`FROM alpine`,
			`RUN set -ex; touch -t @1485449953 /test; chown 65:65 /test`,
			`FROM scratch`,
			`USER 66:66`,
			`COPY --from=0 --chown=1:1 /test /test`,
		}, "\n"),
		fsSkip: []string{"test:mtime"},
	},

	{
		name:       "copy-for-user", // flip side of issue #2518
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM alpine`,
			`USER 66:66`,
			`COPY /script /script`,
		}, "\n"),
	},

	{
		name:       "copy-for-user-with-chown", // flip side of issue #2518, but with chown to override
		contextDir: "copy",
		dockerfileContents: strings.Join([]string{
			`FROM alpine`,
			`USER 66:66`,
			`COPY --chown=1:1 /script /script`,
		}, "\n"),
	},

	{
		name:       "add-parent-symlink",
		contextDir: "add/parent-symlink",
		fsSkip:     []string{"(dir):testsubdir:mtime", "(dir):testsubdir:(dir):etc:mtime"},
	},

	{
		name:       "add-parent-dangling",
		contextDir: "add/parent-dangling",
		fsSkip:     []string{"(dir):symlink:mtime", "(dir):symlink-target:mtime", "(dir):symlink-target:(dir):subdirectory:mtime"},
	},

	{
		name:       "add-parent-clean",
		contextDir: "add/parent-clean",
		fsSkip:     []string{"(dir):symlink:mtime", "(dir):symlink-target:mtime", "(dir):symlink-target:(dir):subdirectory:mtime"},
	},

	{
		name:       "add-archive-1",
		contextDir: "add/archive",
		dockerfile: "Dockerfile.1",
	},

	{
		name:       "add-archive-2",
		contextDir: "add/archive",
		dockerfile: "Dockerfile.2",
	},

	{
		name:       "add-archive-3",
		contextDir: "add/archive",
		dockerfile: "Dockerfile.3",
	},

	{
		name:       "add-archive-4",
		contextDir: "add/archive",
		dockerfile: "Dockerfile.4",
		fsSkip:     []string{"(dir):sub:mtime"},
	},

	{
		name:       "add-archive-5",
		contextDir: "add/archive",
		dockerfile: "Dockerfile.5",
		fsSkip:     []string{"(dir):sub:mtime"},
	},

	{
		name:       "add-archive-6",
		contextDir: "add/archive",
		dockerfile: "Dockerfile.6",
		fsSkip:     []string{"(dir):sub:mtime"},
	},

	{
		name:       "add-archive-7",
		contextDir: "add/archive",
		dockerfile: "Dockerfile.7",
		fsSkip:     []string{"(dir):sub:mtime"},
	},

	{
		name:       "add-dir-not-dir",
		contextDir: "add/dir-not-dir",
	},

	{
		name:       "add-not-dir-dir",
		contextDir: "add/not-dir-dir",
	},

	{
		name:       "add-populated-dir-not-dir",
		contextDir: "add/populated-dir-not-dir",
	},

	{
		name:         "dockerignore-allowlist-subdir-nofile-dir",
		contextDir:   "dockerignore/allowlist/subdir-nofile",
		shouldFailAt: 2,
		failureRegex: "(no such file or directory)|(file not found)|(file does not exist)",
	},

	{
		name:         "dockerignore-allowlist-subdir-nofile-file",
		contextDir:   "dockerignore/allowlist/subdir-nofile",
		shouldFailAt: 2,
		failureRegex: "(no such file or directory)|(file not found)|(file does not exist)",
	},

	{
		name:       "dockerignore-allowlist-subdir-file-dir",
		contextDir: "dockerignore/allowlist/subdir-file",
		fsSkip:     []string{"(dir):f1:mtime"},
	},

	{
		name:       "dockerignore-allowlist-subdir-file-file",
		contextDir: "dockerignore/allowlist/subdir-file",
		fsSkip:     []string{"(dir):f1:mtime"},
	},

	{
		name:       "dockerignore-allowlist-nothing-dot",
		contextDir: "dockerignore/allowlist/nothing-dot",
		fsSkip:     []string{"file:mtime"},
	},

	{
		name:       "dockerignore-allowlist-nothing-slash",
		contextDir: "dockerignore/allowlist/nothing-slash",
		fsSkip:     []string{"file:mtime"},
	},

	{
		// the directories are excluded, so entries for them don't get
		// included in the build context archive, so they only get
		// created implicitly when extracted, so there's no point in us
		// trying to preserve any of that, either
		name:          "dockerignore-allowlist-subsubdir-file",
		contextDir:    "dockerignore/allowlist/subsubdir-file",
		withoutDocker: true,
		fsSkip:        []string{"(dir):folder:mtime", "(dir):folder:(dir):subfolder:mtime", "file:mtime"},
	},

	{
		name:       "dockerignore-allowlist-subsubdir-nofile",
		contextDir: "dockerignore/allowlist/subsubdir-nofile",
		fsSkip:     []string{"file:mtime"},
	},

	{
		name:       "dockerignore-allowlist-subsubdir-nosubdir",
		contextDir: "dockerignore/allowlist/subsubdir-nosubdir",
		fsSkip:     []string{"file:mtime"},
	},

	{
		name:          "dockerignore-allowlist-alternating",
		contextDir:    "dockerignore/allowlist/alternating",
		withoutDocker: true,
		fsSkip: []string{
			"(dir):subdir1:mtime",
			"(dir):subdir1:(dir):subdir2:(dir):subdir3:mtime",
			"(dir):subdir1:(dir):subdir2:(dir):subdir3:(dir):subdir4:(dir):subdir5:mtime",
			"(dir):subdir2:(dir):subdir3:mtime",
			"(dir):subdir2:(dir):subdir3:(dir):subdir4:(dir):subdir5:mtime",
			"(dir):subdir3:mtime",
			"(dir):subdir3:(dir):subdir4:(dir):subdir5:mtime",
			"(dir):subdir4:(dir):subdir5:mtime",
			"(dir):subdir5:mtime",
		},
	},

	{
		name:         "dockerignore-allowlist-alternating-nothing",
		contextDir:   "dockerignore/allowlist/alternating-nothing",
		shouldFailAt: 7,
		failureRegex: "(no such file or directory)|(file not found)|(file does not exist)",
	},

	{
		name:         "dockerignore-allowlist-alternating-other",
		contextDir:   "dockerignore/allowlist/alternating-other",
		shouldFailAt: 7,
		failureRegex: "(no such file or directory)|(file not found)|(file does not exist)",
	},

	{
		name:          "tar-g",
		contextDir:    "tar-g",
		withoutDocker: true,
		fsSkip:        []string{"(dir):tmp:mtime"},
	},

	{
		name:       "dockerignore-exceptions-skip",
		contextDir: "dockerignore/exceptions-skip",
		fsSkip:     []string{"(dir):volume:mtime"},
	},

	{
		name:       "dockerignore-exceptions-weirdness-1",
		contextDir: "dockerignore/exceptions-weirdness-1",
		fsSkip:     []string{"(dir):newdir:mtime", "(dir):newdir:(dir):subdir:mtime"},
	},

	{
		name:       "dockerignore-exceptions-weirdness-2",
		contextDir: "dockerignore/exceptions-weirdness-2",
		fsSkip:     []string{"(dir):newdir:mtime", "(dir):newdir:(dir):subdir:mtime"},
	},

	{
		name:              "multistage-builtin-args",
		dockerfile:        "Dockerfile.margs",
		dockerUseBuildKit: true,
	},

	{
		name:              "heredoc-copy",
		dockerfile:        "Dockerfile.heredoc_copy",
		dockerUseBuildKit: true,
		contextDir:        "heredoc",
		fsSkip: []string{"(dir):test:mtime",
			"(dir):test2:mtime",
			"(dir):test:(dir):humans.txt:mtime",
			"(dir):test:(dir):robots.txt:mtime",
			"(dir):test2:(dir):humans.txt:mtime",
			"(dir):test2:(dir):robots.txt:mtime",
			"(dir):test2:(dir):image_file:mtime",
			"(dir):etc:(dir):hostname" /* buildkit does not contains /etc/hostname like buildah */},
	},

	{
		name:       "replace-symlink-with-directory",
		contextDir: "replace/symlink-with-directory",
	},

	{
		name:       "replace-directory-with-symlink",
		contextDir: "replace/symlink-with-directory",
		dockerfile: "Dockerfile.2",
	},

	{
		name:       "replace-symlink-with-directory-subdir",
		contextDir: "replace/symlink-with-directory",
		dockerfile: "Dockerfile.3",
		fsSkip:     []string{"(dir):tree:mtime"},
	},

	{
		name:       "replace-directory-with-symlink-subdir",
		contextDir: "replace/symlink-with-directory",
		dockerfile: "Dockerfile.4",
		fsSkip:     []string{"(dir):tree:mtime"},
	},

	{
		name: "workdir-owner", // from issue #3620
		dockerfileContents: strings.Join([]string{
			`# syntax=docker/dockerfile:1.4`,
			`FROM alpine`,
			`USER daemon`,
			`WORKDIR /created/directory`,
			`RUN ls /created`,
		}, "\n"),
		fsSkip:            []string{"(dir):created:mtime", "(dir):created:(dir):directory:mtime"},
		dockerUseBuildKit: true,
	},

	{
		name:              "env-precedence",
		contextDir:        "env/precedence",
		dockerUseBuildKit: true,
	},

	{
		name:              "multistage-copyback",
		contextDir:        "multistage/copyback",
		dockerUseBuildKit: true,
	},

	{
		name:              "heredoc-quoting",
		dockerfile:        "Dockerfile.heredoc-quoting",
		dockerUseBuildKit: true,
		fsSkip:            []string{"(dir):etc:(dir):hostname"}, // buildkit does not create a phantom /etc/hostname
	},

	{
		name: "workdir with trailing separator",
		dockerfileContents: strings.Join([]string{
			"FROM busybox",
			"USER daemon",
			"WORKDIR /tmp/",
		}, "\n"),
	},

	{
		name: "workdir without trailing separator",
		dockerfileContents: strings.Join([]string{
			"FROM busybox",
			"USER daemon",
			"WORKDIR /tmp",
		}, "\n"),
	},

	{
		name:             "chown-volume", // from podman #22530
		contextDir:       "chown-volume",
		testUsingVolumes: true,
	},
}

func TestCommit(t *testing.T) {
	testCases := []struct {
		description             string
		baseImage               string
		changes, derivedChanges []string
		config, derivedConfig   *docker.Config
	}{
		{
			description: "defaults",
			baseImage:   "docker.io/library/busybox",
		},
		{
			description: "empty change",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{""},
		},
		{
			description: "empty config",
			baseImage:   "docker.io/library/busybox",
			config:      &docker.Config{},
		},
		{
			description: "cmd just changes",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"CMD /bin/imaginarySh"},
		},
		{
			description: "cmd just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				Cmd: []string{"/usr/bin/imaginarySh"},
			},
		},
		{
			description: "cmd conflict",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"CMD /bin/imaginarySh"},
			config: &docker.Config{
				Cmd: []string{"/usr/bin/imaginarySh"},
			},
		},
		{
			description: "entrypoint just changes",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"ENTRYPOINT /bin/imaginarySh"},
		},
		{
			description: "entrypoint just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				Entrypoint: []string{"/usr/bin/imaginarySh"},
			},
		},
		{
			description: "entrypoint conflict",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"ENTRYPOINT /bin/imaginarySh"},
			config: &docker.Config{
				Entrypoint: []string{"/usr/bin/imaginarySh"},
			},
		},
		{
			description: "environment just changes",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"ENV A=1", "ENV C=2"},
		},
		{
			description: "environment just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				Env: []string{"A=B"},
			},
		},
		{
			description: "environment with conflict union",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"ENV A=1", "ENV C=2"},
			config: &docker.Config{
				Env: []string{"A=B"},
			},
		},
		{
			description: "expose just changes",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"EXPOSE 12345"},
		},
		{
			description: "expose just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				ExposedPorts: map[docker.Port]struct{}{"23456": struct{}{}},
			},
		},
		{
			description: "expose union",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"EXPOSE 12345"},
			config: &docker.Config{
				ExposedPorts: map[docker.Port]struct{}{"23456": struct{}{}},
			},
		},
		{
			description: "healthcheck just changes",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{`HEALTHCHECK --interval=1s --timeout=1s --start-period=1s --retries=1 CMD ["/bin/false"]`},
		},
		{
			description: "healthcheck just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				Healthcheck: &docker.HealthConfig{
					Test:        []string{"/bin/true"},
					Interval:    2 * time.Second,
					Timeout:     2 * time.Second,
					StartPeriod: 2 * time.Second,
					Retries:     2,
				},
			},
		},
		{
			description: "healthcheck conflict",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{`HEALTHCHECK --interval=1s --timeout=1s --start-period=1s --retries=1 CMD ["/bin/false"]`},
			config: &docker.Config{
				Healthcheck: &docker.HealthConfig{
					Test:        []string{"/bin/true"},
					Interval:    2 * time.Second,
					Timeout:     2 * time.Second,
					StartPeriod: 2 * time.Second,
					Retries:     2,
				},
			},
		},
		{
			description: "label just changes",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"LABEL A=1 C=2"},
		},
		{
			description: "label just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				Labels: map[string]string{"A": "B"},
			},
		},
		{
			description: "label with conflict union",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"LABEL A=1 C=2"},
			config: &docker.Config{
				Labels: map[string]string{"A": "B"},
			},
		},
		// n.b. dockerd didn't like a MAINTAINER change, so no test for it, and it's not in a config blob
		{
			description:    "onbuild just changes",
			baseImage:      "docker.io/library/busybox",
			changes:        []string{"ONBUILD USER alice", "ONBUILD LABEL A=1"},
			derivedChanges: []string{"LABEL C=3"},
		},
		{
			description: "onbuild just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				OnBuild: []string{"USER bob", `CMD ["/bin/smash"]`, "LABEL B=2"},
			},
			derivedChanges: []string{"LABEL C=3"},
		},
		{
			description: "onbuild conflict",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"ONBUILD USER alice", "ONBUILD LABEL A=1"},
			config: &docker.Config{
				OnBuild: []string{"USER bob", `CMD ["/bin/smash"]`, "LABEL B=2"},
			},
			derivedChanges: []string{"LABEL C=3"},
		},
		// n.b. dockerd didn't like a SHELL change, so no test for it or a conflict with a config blob
		{
			description: "shell just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				Shell: []string{"/usr/bin/imaginarySh"},
			},
		},
		{
			description: "stop signal conflict",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"STOPSIGNAL SIGTERM"},
			config: &docker.Config{
				StopSignal: "SIGKILL",
			},
		},
		{
			description: "stop timeout=0",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				StopTimeout: 0,
			},
		},
		{
			description: "stop timeout=15",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				StopTimeout: 15,
			},
		},
		{
			description: "stop timeout=15, then 0",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				StopTimeout: 15,
			},
			derivedConfig: &docker.Config{
				StopTimeout: 0,
			},
		},
		{
			description: "stop timeout=0, then 15",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				StopTimeout: 0,
			},
			derivedConfig: &docker.Config{
				StopTimeout: 15,
			},
		},
		{
			description: "user just changes",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"USER 1001:1001"},
		},
		{
			description: "user just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				User: "1000:1000",
			},
		},
		{
			description: "user with conflict",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"USER 1001:1001"},
			config: &docker.Config{
				User: "1000:1000",
			},
		},
		{
			description: "volume just changes",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"VOLUME /a-volume"},
		},
		{
			description: "volume just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				Volumes: map[string]struct{}{"/b-volume": struct{}{}},
			},
		},
		{
			description: "volume union",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"VOLUME /a-volume"},
			config: &docker.Config{
				Volumes: map[string]struct{}{"/b-volume": struct{}{}},
			},
		},
		{
			description: "workdir just changes",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"WORKDIR /yeah"},
		},
		{
			description: "workdir just config",
			baseImage:   "docker.io/library/busybox",
			config: &docker.Config{
				WorkingDir: "/naw",
			},
		},
		{
			description: "workdir with conflict",
			baseImage:   "docker.io/library/busybox",
			changes:     []string{"WORKDIR /yeah"},
			config: &docker.Config{
				WorkingDir: "/naw",
			},
		},
	}

	var tempdir string
	buildahDir := buildahDir
	if buildahDir == "" {
		if tempdir == "" {
			tempdir = t.TempDir()
		}
		buildahDir = filepath.Join(tempdir, "buildah")
	}
	dockerDir := dockerDir
	if dockerDir == "" {
		if tempdir == "" {
			tempdir = t.TempDir()
		}
		dockerDir = filepath.Join(tempdir, "docker")
	}

	ctx := context.TODO()

	// connect to dockerd using go-dockerclient
	client, err := docker.NewClientFromEnv()
	require.NoErrorf(t, err, "unable to initialize docker client")
	var dockerVersion []string
	if version, err := client.Version(); err == nil {
		if version != nil {
			for _, s := range *version {
				dockerVersion = append(dockerVersion, s)
			}
		}
	} else {
		require.NoErrorf(t, err, "unable to connect to docker daemon")
	}

	// find a new place to store buildah builds
	tempdir = t.TempDir()

	// create subdirectories to use for buildah storage
	rootDir := filepath.Join(tempdir, "root")
	runrootDir := filepath.Join(tempdir, "runroot")

	// initialize storage for buildah
	options := storage.StoreOptions{
		GraphDriverName:     os.Getenv("STORAGE_DRIVER"),
		GraphRoot:           rootDir,
		RunRoot:             runrootDir,
		RootlessStoragePath: rootDir,
	}
	store, err := storage.GetStore(options)
	require.NoErrorf(t, err, "error creating buildah storage at %q", rootDir)
	defer func() {
		if store != nil {
			_, err := store.Shutdown(true)
			require.NoErrorf(t, err, "error shutting down storage for buildah")
		}
	}()

	// walk through test cases
	for testIndex, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			test := testCases[testIndex]

			// create the test container, then commit it, using the docker client
			baseImage := test.baseImage
			repository, tag := docker.ParseRepositoryTag(baseImage)
			if tag == "" {
				tag = "latest"
			}
			baseImage = repository + ":" + tag
			if _, err := client.InspectImage(test.baseImage); err != nil && errors.Is(err, docker.ErrNoSuchImage) {
				// oh, we need to pull the base image
				err = client.PullImage(docker.PullImageOptions{
					Repository: repository,
					Tag:        tag,
				}, docker.AuthConfiguration{})
				require.NoErrorf(t, err, "pulling base image")
			}
			container, err := client.CreateContainer(docker.CreateContainerOptions{
				Context: ctx,
				Config: &docker.Config{
					Image: baseImage,
				},
			})
			require.NoErrorf(t, err, "creating the working container with docker")
			if err == nil {
				defer func(containerName string) {
					err := client.RemoveContainer(docker.RemoveContainerOptions{
						ID:    containerName,
						Force: true,
					})
					assert.Nil(t, err, "error deleting working docker container %q", containerName)
				}(container.ID)
			}
			dockerImageName := "committed:" + strconv.Itoa(testIndex)
			dockerImage, err := client.CommitContainer(docker.CommitContainerOptions{
				Container:  container.ID,
				Changes:    test.changes,
				Run:        test.config,
				Repository: dockerImageName,
			})
			assert.NoErrorf(t, err, "committing the working container with docker")
			if err == nil {
				defer func(dockerImageName string) {
					err := client.RemoveImageExtended(dockerImageName, docker.RemoveImageOptions{
						Context: ctx,
						Force:   true,
					})
					assert.Nil(t, err, "error deleting newly-built docker image %q", dockerImage.ID)
				}(dockerImageName)
			}
			dockerRef, err := alltransports.ParseImageName("docker-daemon:" + dockerImageName)
			assert.NoErrorf(t, err, "parsing name of newly-committed docker image")

			if len(test.derivedChanges) > 0 || test.derivedConfig != nil {
				container, err := client.CreateContainer(docker.CreateContainerOptions{
					Context: ctx,
					Config: &docker.Config{
						Image: dockerImage.ID,
					},
				})
				require.NoErrorf(t, err, "creating the derived container with docker")
				if err == nil {
					defer func(containerName string) {
						err := client.RemoveContainer(docker.RemoveContainerOptions{
							ID:    containerName,
							Force: true,
						})
						assert.Nil(t, err, "error deleting derived docker container %q", containerName)
					}(container.ID)
				}
				derivedImageName := "derived:" + strconv.Itoa(testIndex)
				derivedImage, err := client.CommitContainer(docker.CommitContainerOptions{
					Container:  container.ID,
					Changes:    test.derivedChanges,
					Run:        test.derivedConfig,
					Repository: derivedImageName,
				})
				assert.NoErrorf(t, err, "committing the derived container with docker")
				defer func(derivedImageName string) {
					err := client.RemoveImageExtended(derivedImageName, docker.RemoveImageOptions{
						Context: ctx,
						Force:   true,
					})
					assert.Nil(t, err, "error deleting newly-derived docker image %q", derivedImage.ID)
				}(derivedImageName)
				dockerRef, err = alltransports.ParseImageName("docker-daemon:" + derivedImageName)
				assert.NoErrorf(t, err, "parsing name of newly-derived docker image")
			}

			// create the test container, then commit it, using the buildah API
			builder, err := buildah.NewBuilder(ctx, store, buildah.BuilderOptions{
				FromImage: baseImage,
			})
			require.NoErrorf(t, err, "creating the working container with buildah")
			defer func(builder *buildah.Builder) {
				err := builder.Delete()
				assert.NoErrorf(t, err, "removing the working container")
			}(builder)
			var overrideConfig *manifest.Schema2Config
			if test.config != nil {
				overrideConfig = config.Schema2ConfigFromGoDockerclientConfig(test.config)
			}
			buildahID, _, _, err := builder.Commit(ctx, nil, buildah.CommitOptions{
				PreferredManifestType: manifest.DockerV2Schema2MediaType,
				OverrideChanges:       test.changes,
				OverrideConfig:        overrideConfig,
			})
			assert.NoErrorf(t, err, "committing buildah image")
			buildahRef, err := is.Transport.NewStoreReference(store, nil, buildahID)
			assert.NoErrorf(t, err, "parsing name of newly-built buildah image")

			if len(test.derivedChanges) > 0 || test.derivedConfig != nil {
				derivedBuilder, err := buildah.NewBuilder(ctx, store, buildah.BuilderOptions{
					FromImage: buildahID,
				})
				require.NoErrorf(t, err, "creating the derived container with buildah")
				defer func(builder *buildah.Builder) {
					err := builder.Delete()
					assert.NoErrorf(t, err, "removing the derived container")
				}(derivedBuilder)
				var overrideConfig *manifest.Schema2Config
				if test.derivedConfig != nil {
					overrideConfig = config.Schema2ConfigFromGoDockerclientConfig(test.derivedConfig)
				}
				derivedID, _, _, err := builder.Commit(ctx, nil, buildah.CommitOptions{
					PreferredManifestType: manifest.DockerV2Schema2MediaType,
					OverrideChanges:       test.derivedChanges,
					OverrideConfig:        overrideConfig,
				})
				assert.NoErrorf(t, err, "committing derived buildah image")
				buildahRef, err = is.Transport.NewStoreReference(store, nil, derivedID)
				assert.NoErrorf(t, err, "parsing name of newly-derived buildah image")
			}

			// scan the images
			saveReport(ctx, t, dockerRef, filepath.Join(dockerDir, t.Name()), []byte{}, []byte{}, dockerVersion)
			saveReport(ctx, t, buildahRef, filepath.Join(buildahDir, t.Name()), []byte{}, []byte{}, dockerVersion)
			// compare the scans
			_, originalDockerConfig, ociDockerConfig, fsDocker := readReport(t, filepath.Join(dockerDir, t.Name()))
			_, originalBuildahConfig, ociBuildahConfig, fsBuildah := readReport(t, filepath.Join(buildahDir, t.Name()))
			miss, left, diff, same := compareJSON(originalDockerConfig, originalBuildahConfig, originalSkip)
			if !same {
				assert.Failf(t, "Image configurations differ as committed in Docker format", configCompareResult(miss, left, diff, "buildah"))
			}
			miss, left, diff, same = compareJSON(ociDockerConfig, ociBuildahConfig, ociSkip)
			if !same {
				assert.Failf(t, "Image configurations differ when converted to OCI format", configCompareResult(miss, left, diff, "buildah"))
			}
			miss, left, diff, same = compareJSON(fsDocker, fsBuildah, fsSkip)
			if !same {
				assert.Failf(t, "Filesystem contents differ", fsCompareResult(miss, left, diff, "buildah"))
			}
		})
	}
}
