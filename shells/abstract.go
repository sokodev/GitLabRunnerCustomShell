package shells

import (
	"errors"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/common"
)

type AbstractShell struct {
}

func (b *AbstractShell) GetFeatures(features *common.FeaturesInfo) {
	features.Artifacts = true
	features.Cache = true
}

func (b *AbstractShell) writeCdBuildDir(w ShellWriter, info common.ShellScriptInfo) {
	w.Cd(info.Build.FullProjectDir())
}

func (b *AbstractShell) writeExports(w ShellWriter, info common.ShellScriptInfo) {
	for _, variable := range info.Build.GetAllVariables() {
		w.Variable(variable)
	}
}

func (b *AbstractShell) writeGitExports(w ShellWriter, info common.ShellScriptInfo) {
	for _, variable := range info.Build.GetGitTLSVariables() {
		w.Variable(variable)
	}
}

func (b *AbstractShell) writeCloneCmd(w ShellWriter, build *common.Build, projectDir string) {
	templateDir := w.MkTmpDir("git-template")
	args := []string{"clone", "--no-checkout", build.GitInfo.RepoURL, projectDir, "--template", templateDir}

	w.RmDir(projectDir)
	w.Command("git", "config", "-f", path.Join(templateDir, "config"), "fetch.recurseSubmodules", "false")

	if depth := build.GetGitDepth(); depth != "" {
		w.Notice("Cloning repository for %s with git depth set to %s...", build.GitInfo.Ref, depth)
		args = append(args, "--depth", depth, "--branch", build.GitInfo.Ref)
	} else {
		w.Notice("Cloning repository...")
	}

	w.Command("git", args...)
	w.Cd(projectDir)
}

func (b *AbstractShell) writeFetchCmd(w ShellWriter, build *common.Build, projectDir string, gitDir string) {
	depth := build.GetGitDepth()

	w.IfDirectory(gitDir)
	if depth != "" {
		w.Notice("Fetching changes for %s with git depth set to %s...", build.GitInfo.Ref, depth)
	} else {
		w.Notice("Fetching changes...")
	}
	w.Cd(projectDir)
	w.Command("git", "config", "fetch.recurseSubmodules", "false")

	// Remove .git/{index,shallow}.lock files from .git, which can fail the fetch command
	// The file can be left if previous build was terminated during git operation
	w.RmFile(".git/index.lock")
	w.RmFile(".git/shallow.lock")

	w.IfFile(".git/hooks/post-checkout")
	w.RmFile(".git/hooks/post-checkout")
	w.EndIf()

	w.Command("git", "clean", "-ffdx")
	w.Command("git", "reset", "--hard")
	w.Command("git", "remote", "set-url", "origin", build.GitInfo.RepoURL)
	if depth != "" {
		var refspec string
		if build.GitInfo.RefType == common.RefTypeTag {
			refspec = "+refs/tags/" + build.GitInfo.Ref + ":refs/tags/" + build.GitInfo.Ref
		} else {
			refspec = "+refs/heads/" + build.GitInfo.Ref + ":refs/remotes/origin/" + build.GitInfo.Ref
		}
		w.Command("git", "fetch", "--depth", depth, "origin", "--prune", refspec)
	} else {
		w.Command("git", "fetch", "origin", "--prune", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*")
	}
	w.Else()
	b.writeCloneCmd(w, build, projectDir)
	w.EndIf()
}

func (b *AbstractShell) writeCheckoutCmd(w ShellWriter, build *common.Build) {
	w.Notice("Checking out %s as %s...", build.GitInfo.Sha[0:8], build.GitInfo.Ref)
	w.Command("git", "checkout", "-f", "-q", build.GitInfo.Sha)
}

func (b *AbstractShell) writeSubmoduleUpdateCmd(w ShellWriter, build *common.Build, recursive bool) {
	if recursive {
		w.Notice("Updating/initializing submodules recursively...")
	} else {
		w.Notice("Updating/initializing submodules...")
	}

	// Sync .git/config to .gitmodules in case URL changes (e.g. new build token)
	args := []string{"submodule", "sync"}
	if recursive {
		args = append(args, "--recursive")
	}
	w.Command("git", args...)

	// Update / initialize submodules
	args = []string{"submodule", "update", "--init"}
	if recursive {
		args = append(args, "--recursive")
	}
	w.Command("git", args...)
}

func (b *AbstractShell) cacheFile(build *common.Build, userKey string) (key, file string) {
	if build.CacheDir == "" {
		return
	}

	// Deduce cache key
	key = path.Join(build.JobInfo.Name, build.GitInfo.Ref)
	if userKey != "" {
		key = build.GetAllVariables().ExpandValue(userKey)
	}

	// Ignore cache without the key
	if key == "" {
		return
	}

	file = path.Join(build.CacheDir, key, "cache.zip")
	file, err := filepath.Rel(build.BuildDir, file)
	if err != nil {
		return "", ""
	}
	return
}

func (b *AbstractShell) guardRunnerCommand(w ShellWriter, runnerCommand string, action string, f func()) {
	if runnerCommand == "" {
		w.Warning("%s is not supported by this executor.", action)
		return
	}

	w.IfCmd(runnerCommand, "--version")
	f()
	w.Else()
	w.Warning("Missing %s. %s is disabled.", runnerCommand, action)
	w.EndIf()
}

func (b *AbstractShell) cacheExtractor(w ShellWriter, info common.ShellScriptInfo) {
	for _, cacheOptions := range info.Build.Cache {
		// Create list of files to archive
		archiverArgs := []string{}
		for _, path := range cacheOptions.Paths {
			archiverArgs = append(archiverArgs, "--path", path)
		}

		if cacheOptions.Untracked {
			archiverArgs = append(archiverArgs, "--untracked")
		}

		// Skip restoring cache if no cache is defined
		if len(archiverArgs) < 1 {
			continue
		}

		// Skip archiving if no cache is defined
		cacheKey, cacheFile := b.cacheFile(info.Build, cacheOptions.Key)
		if cacheKey == "" {
			continue
		}

		args := []string{
			"cache-extractor",
			"--file", cacheFile,
			"--timeout", strconv.Itoa(info.Build.GetCacheRequestTimeout()),
		}

		// Generate cache download address
		if url := getCacheDownloadURL(info.Build, cacheKey); url != nil {
			args = append(args, "--url", url.String())
		}

		// Execute cache-extractor command. Failure is not fatal.
		b.guardRunnerCommand(w, info.RunnerCommand, "Extracting cache", func() {
			w.Notice("Checking cache for %s...", cacheKey)
			w.IfCmdWithOutput(info.RunnerCommand, args...)
			w.Notice("Successfully extracted cache")
			w.Else()
			w.Warning("Failed to extract cache")
			w.EndIf()
		})
	}
}

func (b *AbstractShell) downloadArtifacts(w ShellWriter, job common.Dependency, info common.ShellScriptInfo) {
	args := []string{
		"artifacts-downloader",
		"--url",
		info.Build.Runner.URL,
		"--token",
		job.Token,
		"--id",
		strconv.Itoa(job.ID),
	}

	w.Notice("Downloading artifacts for %s (%d)...", job.Name, job.ID)
	w.Command(info.RunnerCommand, args...)
}

func (b *AbstractShell) jobArtifacts(info common.ShellScriptInfo) (otherJobs []common.Dependency) {
	for _, otherJob := range info.Build.Dependencies {
		if otherJob.ArtifactsFile.Filename == "" {
			continue
		}

		otherJobs = append(otherJobs, otherJob)
	}
	return
}

func (b *AbstractShell) downloadAllArtifacts(w ShellWriter, info common.ShellScriptInfo) {
	otherJobs := b.jobArtifacts(info)
	if len(otherJobs) == 0 {
		return
	}

	b.guardRunnerCommand(w, info.RunnerCommand, "Artifacts downloading", func() {
		for _, otherJob := range otherJobs {
			b.downloadArtifacts(w, otherJob, info)
		}
	})
}

func (b *AbstractShell) writePrepareScript(w ShellWriter, info common.ShellScriptInfo) (err error) {
	return nil
}

func (b *AbstractShell) writeCloneFetchCmds(w ShellWriter, info common.ShellScriptInfo) (err error) {
	build := info.Build
	projectDir := build.FullProjectDir()
	gitDir := path.Join(build.FullProjectDir(), ".git")

	switch info.Build.GetGitStrategy() {
	case common.GitFetch:
		b.writeFetchCmd(w, build, projectDir, gitDir)
	case common.GitClone:
		b.writeCloneCmd(w, build, projectDir)
	case common.GitNone:
		w.Notice("Skipping Git repository setup")
		w.MkDir(projectDir)
	default:
		return errors.New("unknown GIT_STRATEGY")
	}

	if info.Build.GetGitCheckout() {
		b.writeCheckoutCmd(w, build)
	} else {
		w.Notice("Skippping Git checkout")
	}

	return nil
}

func (b *AbstractShell) writeSubmoduleUpdateCmds(w ShellWriter, info common.ShellScriptInfo) (err error) {
	build := info.Build

	switch build.GetSubmoduleStrategy() {
	case common.SubmoduleNormal:
		b.writeSubmoduleUpdateCmd(w, build, false)

	case common.SubmoduleRecursive:
		b.writeSubmoduleUpdateCmd(w, build, true)

	case common.SubmoduleNone:
		w.Notice("Skipping Git submodules setup")

	default:
		return errors.New("unknown GIT_SUBMODULE_STRATEGY")
	}

	return nil
}

func (b *AbstractShell) writeGetSourcesScript(w ShellWriter, info common.ShellScriptInfo) (err error) {
	b.writeExports(w, info)
	b.writeGitExports(w, info)

	if info.PreCloneScript != "" && info.Build.GetGitStrategy() != common.GitNone {
		b.writeCommands(w, info.PreCloneScript)
	}

	if err := b.writeCloneFetchCmds(w, info); err != nil {
		return err
	}

	if err = b.writeSubmoduleUpdateCmds(w, info); err != nil {
		return err
	}

	return nil
}

func (b *AbstractShell) writeRestoreCacheScript(w ShellWriter, info common.ShellScriptInfo) (err error) {
	b.writeExports(w, info)
	b.writeCdBuildDir(w, info)

	// Try to restore from main cache, if not found cache for master
	b.cacheExtractor(w, info)
	return nil
}

func (b *AbstractShell) writeDownloadArtifactsScript(w ShellWriter, info common.ShellScriptInfo) (err error) {
	b.writeExports(w, info)
	b.writeCdBuildDir(w, info)

	// Process all artifacts
	b.downloadAllArtifacts(w, info)
	return nil
}

// Write the given string of commands using the provided ShellWriter object.
func (b *AbstractShell) writeCommands(w ShellWriter, commands string) {
	commands = strings.TrimSpace(commands)
	for _, command := range strings.Split(commands, "\n") {
		command = strings.TrimSpace(command)
		if command != "" {
			w.Notice("$ %s", command)
		} else {
			w.EmptyLine()
		}
		w.Line(command)
		w.CheckForErrors()
	}
}

func (b *AbstractShell) writeUserScript(w ShellWriter, info common.ShellScriptInfo) (err error) {
	var scriptStep *common.Step
	for _, step := range info.Build.Steps {
		if step.Name == common.StepNameScript {
			scriptStep = &step
			break
		}
	}

	if scriptStep == nil {
		return nil
	}

	b.writeExports(w, info)
	b.writeCdBuildDir(w, info)

	if info.PreBuildScript != "" {
		b.writeCommands(w, info.PreBuildScript)
	}

	commands := strings.Join(scriptStep.Script, "\n")
	b.writeCommands(w, commands)

	if info.PostBuildScript != "" {
		b.writeCommands(w, info.PostBuildScript)
	}

	return nil
}

func (b *AbstractShell) cacheArchiver(w ShellWriter, info common.ShellScriptInfo) {
	for _, cacheOptions := range info.Build.Cache {
		// Skip archiving if no cache is defined
		cacheKey, cacheFile := b.cacheFile(info.Build, cacheOptions.Key)
		if cacheKey == "" {
			continue
		}

		args := []string{
			"cache-archiver",
			"--file", cacheFile,
			"--timeout", strconv.Itoa(info.Build.GetCacheRequestTimeout()),
		}

		// Create list of files to archive
		archiverArgs := []string{}
		for _, path := range cacheOptions.Paths {
			archiverArgs = append(archiverArgs, "--path", path)
		}

		if cacheOptions.Untracked {
			archiverArgs = append(archiverArgs, "--untracked")
		}

		if len(archiverArgs) < 1 {
			// Skip creating archive
			continue
		}
		args = append(args, archiverArgs...)

		// Generate cache upload address
		if url := getCacheUploadURL(info.Build, cacheKey); url != nil {
			args = append(args, "--url", url.String())
		}

		// Execute cache-archiver command. Failure is not fatal.
		b.guardRunnerCommand(w, info.RunnerCommand, "Creating cache", func() {
			w.Notice("Creating cache %s...", cacheKey)
			w.IfCmdWithOutput(info.RunnerCommand, args...)
			w.Notice("Created cache")
			w.Else()
			w.Warning("Failed to create cache")
			w.EndIf()
		})
	}
}

func (b *AbstractShell) uploadArtifacts(w ShellWriter, info common.ShellScriptInfo) {
	if info.Build.Runner.URL == "" {
		return
	}

	for _, artifacts := range info.Build.Artifacts {
		args := []string{
			"artifacts-uploader",
			"--url",
			info.Build.Runner.URL,
			"--token",
			info.Build.Token,
			"--id",
			strconv.Itoa(info.Build.ID),
		}

		// Create list of files to archive
		archiverArgs := []string{}
		for _, path := range artifacts.Paths {
			archiverArgs = append(archiverArgs, "--path", path)
		}

		if artifacts.Untracked {
			archiverArgs = append(archiverArgs, "--untracked")
		}

		if len(archiverArgs) < 1 {
			// Skip creating archive
			continue
		}
		args = append(args, archiverArgs...)

		if artifacts.Name != "" {
			args = append(args, "--name", artifacts.Name)
		}

		if artifacts.ExpireIn != "" {
			args = append(args, "--expire-in", artifacts.ExpireIn)
		}

		b.guardRunnerCommand(w, info.RunnerCommand, "Uploading artifacts", func() {
			w.Notice("Uploading artifacts...")
			w.Command(info.RunnerCommand, args...)
		})
	}
}

func (b *AbstractShell) writeAfterScript(w ShellWriter, info common.ShellScriptInfo) error {
	var afterScriptStep *common.Step
	for _, step := range info.Build.Steps {
		if step.Name == common.StepNameAfterScript {
			afterScriptStep = &step
			break
		}
	}

	if afterScriptStep == nil {
		return nil
	}

	b.writeExports(w, info)
	b.writeCdBuildDir(w, info)

	w.Notice("Running after script...")

	for _, command := range afterScriptStep.Script {
		command = strings.TrimSpace(command)
		if command != "" {
			w.Notice("$ %s", command)
		} else {
			w.EmptyLine()
		}
		w.Line(command)
		w.CheckForErrors()
	}

	return nil
}

func (b *AbstractShell) writeArchiveCacheScript(w ShellWriter, info common.ShellScriptInfo) (err error) {
	b.writeExports(w, info)
	b.writeCdBuildDir(w, info)

	// Find cached files and archive them
	b.cacheArchiver(w, info)
	return
}

func (b *AbstractShell) writeUploadArtifactsScript(w ShellWriter, info common.ShellScriptInfo) (err error) {
	b.writeExports(w, info)
	b.writeCdBuildDir(w, info)

	// Upload artifacts
	b.uploadArtifacts(w, info)
	return
}

func (b *AbstractShell) writeScript(w ShellWriter, buildStage common.BuildStage, info common.ShellScriptInfo) error {
	methods := map[common.BuildStage]func(ShellWriter, common.ShellScriptInfo) error{
		common.BuildStagePrepare:           b.writePrepareScript,
		common.BuildStageGetSources:        b.writeGetSourcesScript,
		common.BuildStageRestoreCache:      b.writeRestoreCacheScript,
		common.BuildStageDownloadArtifacts: b.writeDownloadArtifactsScript,
		common.BuildStageUserScript:        b.writeUserScript,
		common.BuildStageAfterScript:       b.writeAfterScript,
		common.BuildStageArchiveCache:      b.writeArchiveCacheScript,
		common.BuildStageUploadArtifacts:   b.writeUploadArtifactsScript,
	}

	fn := methods[buildStage]
	if fn == nil {
		return errors.New("Not supported script type: " + string(buildStage))
	}

	return fn(w, info)
}
