package tiltfile

import (
	"context"
	"fmt"
	"io/ioutil"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/windmilleng/tilt/internal/k8s"
	"github.com/windmilleng/tilt/internal/logger"

	"github.com/docker/distribution/reference"
	"github.com/google/skylark"
	"github.com/google/skylark/resolve"
	"github.com/pkg/errors"
	"github.com/windmilleng/tilt/internal/kustomize"
	"github.com/windmilleng/tilt/internal/model"
	"github.com/windmilleng/tilt/internal/ospath"
)

const FileName = "Tiltfile"

const oldK8sServiceSyntaxError = "the syntax for `k8s_service` has changed. Before it was `k8s_service(yaml: string, dockerImage: Image)`. " +
	"Now it is `k8s_service(dockerImage: Image, yaml: string = \"\")` (`yaml` is an optional arg)."

type Tiltfile struct {
	globals skylark.StringDict
	thread  *skylark.Thread

	// The filename we're executing. Must be absolute.
	filename string
}

func init() {
	resolve.AllowLambda = true
	resolve.AllowNestedDef = true
}

func (t *Tiltfile) makeSkylarkDockerImage(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var dockerfileName skylark.Value
	var entrypoint, dockerRef string
	err := skylark.UnpackArgs(fn.Name(), args, kwargs,
		"docker_file_name", &dockerfileName,
		"docker_file_tag", &dockerRef,
		"entrypoint?", &entrypoint,
	)
	if err != nil {
		return nil, err
	}

	dockerfileLocalPath, err := t.localPathFromSkylarkValue(dockerfileName)
	if err != nil {
		return nil, fmt.Errorf("Argument 0 (docker_file_name): %v", err)
	}

	ref, err := reference.ParseNormalizedNamed(dockerRef)
	if err != nil {
		return nil, fmt.Errorf("Parsing %q: %v", dockerRef, err)
	}

	existingBC := thread.Local(buildContextKey)

	if existingBC != nil {
		return skylark.None, errors.New("tried to start a build context while another build context was already open")
	}

	buildContext := &dockerImage{
		baseDockerfilePath: dockerfileLocalPath,
		ref:                ref,
		entrypoint:         entrypoint,
		tiltFilename:       t.filename,
	}
	err = t.recordReadFile(thread, dockerfileLocalPath.path)
	if err != nil {
		return skylark.None, err
	}
	thread.SetLocal(buildContextKey, buildContext)
	return skylark.None, nil
}

func skylarkStringDictToGoMap(d *skylark.Dict) (map[string]string, error) {
	r := map[string]string{}

	for _, tuple := range d.Items() {
		kV, ok := tuple[0].(skylark.String)
		if !ok {
			return nil, fmt.Errorf("key is not a string: %T (%v)", tuple[0], tuple[0])
		}

		k := string(kV)

		vV, ok := tuple[1].(skylark.String)
		if !ok {
			return nil, fmt.Errorf("value is not a string: %T (%v)", tuple[1], tuple[1])
		}

		v := string(vV)

		r[k] = v
	}

	return r, nil
}

func (t *Tiltfile) makeStaticBuild(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var dockerRef string
	var dockerfilePath, buildPath, buildArgs skylark.Value
	err := skylark.UnpackArgs(fn.Name(), args, kwargs,
		"dockerfile", &dockerfilePath,
		"ref", &dockerRef,
		"build_args?", &buildArgs,
		"context?", &buildPath,
	)
	if err != nil {
		return nil, err
	}

	ref, err := reference.ParseNormalizedNamed(dockerRef)
	if err != nil {
		return nil, fmt.Errorf("Parsing %q: %v", dockerRef, err)
	}

	dockerfileLocalPath, err := t.localPathFromSkylarkValue(dockerfilePath)
	if err != nil {
		return nil, fmt.Errorf("Argument 0 (dockerfile): %v", err)
	}

	var sba map[string]string
	if buildArgs != nil {
		d, ok := buildArgs.(*skylark.Dict)
		if !ok {
			return nil, fmt.Errorf("Argument 2 (build_args): expected dict, got %T", buildArgs)
		}

		sba, err = skylarkStringDictToGoMap(d)
		if err != nil {
			return nil, fmt.Errorf("Argument 2 (build_args): %v", err)
		}
	}

	var buildLocalPath localPath
	if buildPath == nil {
		buildLocalPath = localPath{
			path: filepath.Dir(dockerfileLocalPath.path),
			repo: dockerfileLocalPath.repo,
		}
	} else {
		buildLocalPath, err = t.localPathFromSkylarkValue(buildPath)
		if err != nil {
			return nil, fmt.Errorf("Argument 3 (context): %v", err)
		}
	}

	buildContext := &dockerImage{
		staticDockerfilePath: dockerfileLocalPath,
		staticBuildPath:      buildLocalPath,
		ref:                  ref,
		tiltFilename:         t.filename,
		staticBuildArgs:      sba,
	}
	err = t.recordReadFile(thread, dockerfileLocalPath.path)
	if err != nil {
		return skylark.None, err
	}
	return buildContext, nil
}

func unimplementedSkylarkFunction(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	return skylark.None, errors.New(fmt.Sprintf("%s not implemented", fn.Name()))
}

func makeSkylarkK8Manifest(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var dockerImage *dockerImage
	var yaml skylark.String
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "dockerImage", &dockerImage, "yaml?", &yaml)
	if err != nil {
		if strings.Contains(err.Error(), "got string, want dockerImage") {
			return nil, fmt.Errorf(oldK8sServiceSyntaxError)
		}
		return nil, err
	}

	// Name will be initialized later
	return &k8sManifest{
		k8sYaml:     yaml,
		dockerImage: *dockerImage,
	}, nil
}

func (t *Tiltfile) makeSkylarkCompositeManifest(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {

	var manifestFuncs skylark.Iterable
	err := skylark.UnpackArgs(fn.Name(), args, kwargs,
		"services", &manifestFuncs)
	if err != nil {
		return nil, err
	}

	var manifests []*k8sManifest

	var v skylark.Value
	i := manifestFuncs.Iterate()
	defer i.Done()
	for i.Next(&v) {
		switch v := v.(type) {
		case *skylark.Function:
			thread.SetLocal(readFilesKey, []string{})
			r, err := v.Call(thread, nil, nil)
			if err != nil {
				return nil, handleSkylarkErr(t.thread, err)
			}
			s, ok := r.(*k8sManifest)
			if !ok {
				return nil, fmt.Errorf("composite_service: function %v returned %v %T; expected k8s_service", v.Name(), r, r)
			}

			files, err := getAndClearReadFiles(thread)
			if err != nil {
				return nil, err
			}

			s.name = v.Name()
			s.configFiles = files

			manifests = append(manifests, s)
		default:
			return nil, fmt.Errorf("composite_service: unexpected input %v %T", v, v)
		}
	}
	return compManifest{manifests}, nil
}

func (t *Tiltfile) makeSkylarkGitRepo(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var path string
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "path", &path)
	if err != nil {
		return nil, err
	}

	repo, err := t.newGitRepo(path)
	if err != nil {
		return nil, err
	}

	return repo, nil
}

func runLocalCmd(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var command string
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "command", &command)
	if err != nil {
		return nil, err
	}

	out, err := execLocalCmd(command)
	if err != nil {
		return nil, err
	}

	return skylark.String(out), nil
}

func execLocalCmd(cmd string) (string, error) {
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		errorMessage := fmt.Sprintf("command '%v' failed.\nerror: '%v'\nstdout: '%v'", cmd, err, string(out))
		exitError, ok := err.(*exec.ExitError)
		if ok {
			errorMessage += fmt.Sprintf("\nstderr: '%v'", string(exitError.Stderr))
		}
		return "", errors.New(errorMessage)
	}

	return string(out), nil
}

// When running the Tilt demo, the current working directory is arbitrary.
// So we want to resolve paths relative to the dir where the Tiltfile lives,
// not relative to the working directory.
func (t *Tiltfile) absPath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(t.absWorkingDir(), path)
}

func (t *Tiltfile) absWorkingDir() string {
	return filepath.Dir(t.filename)
}

func (t *Tiltfile) readFile(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var path string
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "path", &path)
	if err != nil {
		return nil, err
	}

	path = t.absPath(path)
	dat, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	err = t.recordReadFile(thread, path)
	if err != nil {
		return nil, err
	}

	return skylark.String(dat), nil
}

func stopBuild(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	buildContext, err := getAndClearBuildContext(thread)
	if err != nil {
		return nil, err
	} else if buildContext == nil {
		return nil, errors.New(noActiveBuildError)
	}
	return buildContext, nil
}

func (t *Tiltfile) callKustomize(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	var path skylark.Value
	err := skylark.UnpackArgs(fn.Name(), args, kwargs, "path", &path)
	if err != nil {
		return nil, err
	}

	kustomizePath, err := t.localPathFromSkylarkValue(path)
	if err != nil {
		return nil, fmt.Errorf("Argument 0 (path): %v", err)
	}

	cmd := fmt.Sprintf("kustomize build %s", path)
	yaml, err := execLocalCmd(cmd)
	if err != nil {
		return nil, err
	}
	deps, err := kustomize.Deps(kustomizePath.String())
	if err != nil {
		return nil, fmt.Errorf("internal error: %v", err)
	}
	for _, d := range deps {
		err := t.recordReadFile(thread, d)
		if err != nil {
			return nil, err
		}
	}

	return skylark.String(yaml), nil
}

func (t *Tiltfile) globalYaml(thread *skylark.Thread, fn *skylark.Builtin, args skylark.Tuple, kwargs []skylark.Tuple) (skylark.Value, error) {
	yaml, err := getGlobalYAML(thread)
	if err != nil {
		return nil, errors.Wrap(err, "checking if globalYAML already set")
	}
	if yaml != "" {
		return nil, fmt.Errorf("`global_yaml` can be called only once per Tiltfile")
	}

	err = skylark.UnpackArgs(fn.Name(), args, kwargs, "yaml", &yaml)
	if err != nil {
		return nil, err
	}

	deps, err := getReadFiles(thread)
	if err != nil {
		return nil, err
	}

	setGlobalYAML(thread, yaml)
	setGlobalYAMLDeps(thread, deps)

	return skylark.None, nil
}

func Load(ctx context.Context, filename string) (*Tiltfile, error) {
	thread := &skylark.Thread{
		Print: func(_ *skylark.Thread, msg string) {
			logger.Get(ctx).Infof("%s", msg)
		},
	}

	filename, err := ospath.RealAbs(filename)
	if err != nil {
		return nil, err
	}

	tiltfile := &Tiltfile{
		filename: filename,
		thread:   thread,
	}

	predeclared := skylark.StringDict{
		"start_fast_build":  skylark.NewBuiltin("start_fast_build", tiltfile.makeSkylarkDockerImage),
		"start_slow_build":  skylark.NewBuiltin("start_slow_build", unimplementedSkylarkFunction),
		"static_build":      skylark.NewBuiltin("static_build", tiltfile.makeStaticBuild),
		"k8s_service":       skylark.NewBuiltin("k8s_service", makeSkylarkK8Manifest),
		"local_git_repo":    skylark.NewBuiltin("local_git_repo", tiltfile.makeSkylarkGitRepo),
		"local":             skylark.NewBuiltin("local", runLocalCmd),
		"composite_service": skylark.NewBuiltin("composite_service", tiltfile.makeSkylarkCompositeManifest),
		"read_file":         skylark.NewBuiltin("read_file", tiltfile.readFile),
		"stop_build":        skylark.NewBuiltin("stop_build", stopBuild),
		"add":               skylark.NewBuiltin("add", addMount),
		"run":               skylark.NewBuiltin("run", tiltfile.runDockerImageCmd),
		"kustomize":         skylark.NewBuiltin("kustomize", tiltfile.callKustomize),
		"global_yaml":       skylark.NewBuiltin("global_yaml", tiltfile.globalYaml),
	}

	globals, err := skylark.ExecFile(thread, filename, nil, predeclared)
	if err != nil {
		return nil, handleSkylarkErr(thread, err)
	}

	tiltfile.globals = globals
	return tiltfile, nil
}

func handleSkylarkErr(thread *skylark.Thread, err error) error {
	evalErr, isEvalErr := err.(*skylark.EvalError)
	if isEvalErr {
		return fmt.Errorf("%s\n\n%s", evalErr.Error(), evalErr.Backtrace())
	}

	return err
}

// GetManifestConfigsAndGlobalYAML executes the Tiltfile to create manifests for all resources and
// a manifest representing the global yaml.
func (t Tiltfile) GetManifestConfigsAndGlobalYAML(ctx context.Context, names ...model.ManifestName) ([]model.Manifest, model.YAMLManifest, error) {
	var manifests []model.Manifest

	gYAMLDeps, err := getGlobalYAMLDeps(t.thread)
	if err != nil {
		return nil, model.YAMLManifest{}, err
	}

	for _, manifestName := range names {
		curManifests, err := t.getManifestConfigsHelper(ctx, manifestName.String())
		if err != nil {
			return manifests, model.YAMLManifest{}, err
		}

		// All manifests depend on global YAML, therefore all depend on its dependencies.
		// TODO(maia): there's probs a better thread-magic way for each individual manifest to
		// about files opened in the global scope, i.e. files opened when getting global YAML.
		for i, m := range curManifests {
			deps := append(m.ConfigFiles, gYAMLDeps...)
			curManifests[i] = m.WithConfigFiles(deps)
		}

		manifests = append(manifests, curManifests...)
	}

	gYAML, err := getGlobalYAML(t.thread)
	if err != nil {
		return nil, model.YAMLManifest{}, err
	}
	globalYAML := model.NewYAMLManifest(model.GlobalYAMLManifestName, gYAML, gYAMLDeps)

	return manifests, globalYAML, nil
}

func (t Tiltfile) getManifestConfigsHelper(ctx context.Context, manifestName string) ([]model.Manifest, error) {
	f, ok := t.globals[manifestName]

	if !ok {
		var globalNames []string
		for name := range t.globals {
			globalNames = append(globalNames, name)
		}

		return nil, fmt.Errorf(
			"%s does not define a global named '%v'. perhaps you want one of:\n  %s",
			t.filename,
			manifestName,
			strings.Join(globalNames, "\n  "))
	}

	manifestFunction, ok := f.(*skylark.Function)

	if !ok {
		return nil, fmt.Errorf("'%v' is a '%v', not a function. service definitions must be functions", manifestName, f.Type())
	}

	if manifestFunction.NumParams() != 0 {
		return nil, fmt.Errorf("func '%v' is defined to take more than 0 arguments. service definitions must take 0 arguments", manifestName)
	}

	thread := t.thread
	thread.SetLocal(readFilesKey, []string{})

	val, err := manifestFunction.Call(t.thread, nil, nil)
	if err != nil {
		return nil, handleSkylarkErr(t.thread, err)
	}

	files, err := getAndClearReadFiles(thread)
	if err != nil {
		return nil, err
	}

	switch manifest := val.(type) {
	case compManifest:
		var manifests []model.Manifest

		for _, cMan := range manifest.cManifest {
			m, err := skylarkManifestToDomain(cMan)
			if err != nil {
				return nil, err
			}

			manifests = append(manifests, m)
		}
		return manifests, nil
	case *k8sManifest:
		manifest.configFiles = files

		m, err := skylarkManifestToDomain(manifest)
		if err != nil {
			return nil, err
		}

		m.Name = model.ManifestName(manifestName)

		manifestYAMLFromGlobalYAML, err := t.extractFromGlobalYAMLForManifest(ctx, m)
		if err != nil {
			return nil, errors.Wrapf(err, "extracting global yaml for manifest %s", m.Name)
		}
		m = m.AppendK8sYAML(manifestYAMLFromGlobalYAML)

		return []model.Manifest{m}, nil

	default:
		return nil, fmt.Errorf("'%v' returned a '%v', but it needs to return a k8s_service or composite_service", manifestName, val.Type())
	}
}

// extractFromGlobalYAMLForManifest finds any objects defined in the global YAML
// that correspond to the given manifest, and extracts and returns them. (Note
// that this operation modifies the global YAML in place!)
func (t *Tiltfile) extractFromGlobalYAMLForManifest(ctx context.Context, m model.Manifest) (string, error) {
	gYAML, err := getGlobalYAML(t.thread)
	if err != nil {
		return "", err
	}
	entities, err := k8s.ParseYAMLFromString(gYAML)
	if err != nil {
		return "", errors.Wrap(err, "parsing global yaml")
	}

	var matchingSelector []k8s.K8sEntity
	// TODO(maia): also get entities that select for any of THESE entities (services etc.)
	matchingImg, allRest, err := k8s.FilterByImage(entities, m.DockerRef())
	for _, e := range matchingImg {
		podTemplates, err := k8s.ExtractPodTemplateSpec(e)
		if err != nil {
			return "", errors.Wrap(err, "extracting pod template spec")
		}
		if len(podTemplates) == 0 {
			continue
		}

		if len(podTemplates) > 1 {
			logger.Get(ctx).Infof("Found multiple pod templates on your %s for manifest %s, "+
				"looking for services that match the first one", e.Kind.Kind, m.Name)
		}
		template := podTemplates[0]

		match, rest, err := k8s.FilterByLabels(allRest, template.Labels)
		if err != nil {
			return "", errors.Wrap(err, "filtering entities by label")
		}
		matchingSelector = append(matchingSelector, match...)
		allRest = rest
	}

	matching := append(matchingImg, matchingSelector...)

	// GlobalYAML = GlobalYAML without any k8s entries matching this manifest
	gYAMLWithoutMatches, err := k8s.SerializeYAML(allRest)
	if err != nil {
		return "", errors.Wrap(err, "re-serializing global yaml")
	}
	setGlobalYAML(t.thread, gYAMLWithoutMatches)

	matchingYAML, err := k8s.SerializeYAML(matching)
	if err != nil {
		return "", errors.Wrapf(err, "serializing yaml extracted for manifest %s", m.Name)
	}
	return matchingYAML, nil
}

func skylarkManifestToDomain(manifest *k8sManifest) (model.Manifest, error) {
	k8sYaml, ok := skylark.AsString(manifest.k8sYaml)
	if !ok {
		return model.Manifest{}, fmt.Errorf("internal error: k8sService.k8sYaml was not a string in '%v'", manifest)
	}

	var err error
	image := manifest.dockerImage
	baseDockerfileBytes := []byte{}
	staticDockerfileBytes := []byte{}
	if image.staticDockerfilePath.Truth() {
		staticDockerfileBytes, err = ioutil.ReadFile(image.staticDockerfilePath.path)
		if err != nil {
			return model.Manifest{}, fmt.Errorf("failed to open dockerfile '%v': %v", image.staticDockerfilePath.path, err)
		}
	} else {
		baseDockerfileBytes, err = ioutil.ReadFile(image.baseDockerfilePath.path)
		if err != nil {
			return model.Manifest{}, fmt.Errorf("failed to open dockerfile '%v': %v", image.baseDockerfilePath.path, err)
		}
	}

	m := model.Manifest{
		BaseDockerfile: string(baseDockerfileBytes),
		Mounts:         skylarkMountsToDomain(image.mounts),
		Steps:          image.steps,
		Entrypoint:     model.ToShellCmd(image.entrypoint),
		Name:           model.ManifestName(manifest.name),
		ConfigFiles:    SkylarkConfigFilesToDomain(manifest.configFiles),

		StaticDockerfile: string(staticDockerfileBytes),
		StaticBuildPath:  string(image.staticBuildPath.path),
		StaticBuildArgs:  image.staticBuildArgs,

		Repos: SkylarkReposToDomain(image),
	}

	m = m.WithPortForwards(manifest.portForwards).WithTiltFilename(image.tiltFilename).WithK8sYAML(k8sYaml).WithDockerRef(image.ref)

	return m, nil
}

func SkylarkConfigFilesToDomain(cf []string) []string {
	ss := sort.StringSlice(cf)
	ss.Sort()

	return ss
}

func SkylarkReposToDomain(image dockerImage) []model.LocalGithubRepo {
	dRepos := []model.LocalGithubRepo{}
	repoSet := make(map[string]bool, 0)

	maybeAddRepo := func(repo gitRepo) {
		if !repo.Truth() {
			return
		}

		if repoSet[repo.basePath] {
			return
		}

		repoSet[repo.basePath] = true
		dRepos = append(dRepos, model.LocalGithubRepo{
			LocalPath:            repo.basePath,
			DockerignoreContents: repo.dockerignoreContents,
			GitignoreContents:    repo.gitignoreContents,
		})
	}

	for _, m := range image.mounts {
		maybeAddRepo(m.repo)
	}
	maybeAddRepo(image.baseDockerfilePath.repo)
	maybeAddRepo(image.staticDockerfilePath.repo)
	maybeAddRepo(image.staticBuildPath.repo)

	return dRepos
}

func skylarkMountsToDomain(sMounts []mount) []model.Mount {
	dMounts := make([]model.Mount, len(sMounts))
	for i, m := range sMounts {
		dMounts[i] = model.Mount{
			LocalPath:     m.src.path,
			ContainerPath: m.mountPoint,
		}
	}
	return dMounts
}
