package cmd

import (
	"bytes"
	"github.com/SAP/jenkins-library/pkg/command"
	"github.com/SAP/jenkins-library/pkg/docker"
	gitUtil "github.com/SAP/jenkins-library/pkg/git"
	"github.com/SAP/jenkins-library/pkg/log"
	"github.com/SAP/jenkins-library/pkg/piperutils"
	"github.com/SAP/jenkins-library/pkg/telemetry"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/pkg/errors"
	"io"
	"os"
	"path/filepath"
)

type iGitopsUpdateDeploymentGitUtils interface {
	CommitSingleFile(filePath, commitMessage, author string) (plumbing.Hash, error)
	PushChangesToRepository(username, password string) error
	PlainClone(username, password, serverURL, directory string) error
	ChangeBranch(branchName string) error
}

type gitopsUpdateDeploymentFileUtils interface {
	TempDir(dir, pattern string) (name string, err error)
	RemoveAll(path string) error
	FileWrite(path string, content []byte, perm os.FileMode) error
	Getwd() (string, error)
}

type gitopsUpdateDeploymentExecRunner interface {
	RunExecutable(executable string, params ...string) error
	Stdout(out io.Writer)
	Stderr(err io.Writer)
}

type gitopsUpdateDeploymentGitUtils struct {
	worktree   *git.Worktree
	repository *git.Repository
}

func (g *gitopsUpdateDeploymentGitUtils) CommitSingleFile(filePath, commitMessage, author string) (plumbing.Hash, error) {
	return gitUtil.CommitSingleFile(filePath, commitMessage, author, g.worktree)
}

func (g *gitopsUpdateDeploymentGitUtils) PushChangesToRepository(username, password string) error {
	return gitUtil.PushChangesToRepository(username, password, g.repository)
}

func (g *gitopsUpdateDeploymentGitUtils) PlainClone(username, password, serverURL, directory string) error {
	var err error
	g.repository, err = gitUtil.PlainClone(username, password, serverURL, directory)
	if err != nil {
		return errors.Wrap(err, "plain clone failed")
	}
	g.worktree, err = g.repository.Worktree()
	return errors.Wrap(err, "failed to retrieve worktree")
}

func (g *gitopsUpdateDeploymentGitUtils) ChangeBranch(branchName string) error {
	return gitUtil.ChangeBranch(branchName, g.worktree)
}

func gitopsUpdateDeployment(config gitopsUpdateDeploymentOptions, _ *telemetry.CustomData) {
	// for command execution use Command
	var c gitopsUpdateDeploymentExecRunner = &command.Command{}
	// reroute command output to logging framework
	c.Stdout(log.Writer())
	c.Stderr(log.Writer())

	// for http calls import  piperhttp "github.com/SAP/jenkins-library/pkg/http"
	// and use a  &piperhttp.Client{} in a custom system
	// Example: step checkmarxExecuteScan.go

	// error situations should stop execution through log.Entry().Fatal() call which leads to an os.Exit(1) in the end
	err := runGitopsUpdateDeployment(&config, c, &gitopsUpdateDeploymentGitUtils{}, piperutils.Files{})
	if err != nil {
		log.Entry().WithError(err).Fatal("step execution failed")
	}
}

func runGitopsUpdateDeployment(config *gitopsUpdateDeploymentOptions, command gitopsUpdateDeploymentExecRunner, gitUtils iGitopsUpdateDeploymentGitUtils, fileUtils gitopsUpdateDeploymentFileUtils) error {
	workingDirectory, err := fileUtils.Getwd()
	if err != nil {
		return errors.Wrap(err, "failed to get working directory")
	}
	temporaryFolder, err := fileUtils.TempDir(workingDirectory, "temp-")
	if err != nil {
		return errors.Wrap(err, "failed to create temporary directory")
	}

	defer func() {
		err = fileUtils.RemoveAll(temporaryFolder)
		if err != nil {
			log.Entry().WithError(err).Error("error during temporary directory deletion")
		}
	}()

	err = cloneRepositoryAndChangeBranch(config, gitUtils, temporaryFolder)
	if err != nil {
		return errors.Wrap(err, "repository could not get prepared")
	}

	filePath := filepath.Join(temporaryFolder, config.FilePath)

	var outputBytes []byte
	if config.DeployTool == "kubectl" {
		outputBytes, err = executeKubectl(config, command, outputBytes, filePath)
		if err != nil {
			return errors.Wrap(err, "error on kubectl execution")
		}
	} else if config.DeployTool == "helm" {
		outputBytes, err = runHelmCommand(command, config, workingDirectory)
		if err != nil {
			return errors.Wrap(err, "failed to apply helm command")
		}
	} else {
		return errors.New("deploy tool " + config.DeployTool + " is not supported")
	}

	err = fileUtils.FileWrite(filePath, outputBytes, 0755)
	if err != nil {
		return errors.Wrap(err, "failed to write file")
	}

	commit, err := commitAndPushChanges(config, gitUtils)
	if err != nil {
		return errors.Wrap(err, "failed to commit and push changes")
	}

	log.Entry().Infof("Changes committed with %s", commit.String())

	return nil
}

func executeKubectl(config *gitopsUpdateDeploymentOptions, command gitopsUpdateDeploymentExecRunner, outputBytes []byte, filePath string) ([]byte, error) {
	registryImage, err := buildRegistryPlusImage(config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to apply kubectl command")
	}
	patchString := "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"" + config.ContainerName + "\",\"image\":\"" + registryImage + "\"}]}}}}"

	outputBytes, err = runKubeCtlCommand(command, patchString, filePath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to apply kubectl command")
	}
	return outputBytes, nil
}

func cloneRepositoryAndChangeBranch(config *gitopsUpdateDeploymentOptions, gitUtils iGitopsUpdateDeploymentGitUtils, temporaryFolder string) error {
	err := gitUtils.PlainClone(config.Username, config.Password, config.ServerURL, temporaryFolder)
	if err != nil {
		return errors.Wrap(err, "failed to plain clone repository")
	}

	err = gitUtils.ChangeBranch(config.BranchName)
	if err != nil {
		return errors.Wrap(err, "failed to change branch")
	}
	return nil
}

func runKubeCtlCommand(command gitopsUpdateDeploymentExecRunner, patchString string, filePath string) ([]byte, error) {
	var kubectlOutput = bytes.Buffer{}
	command.Stdout(&kubectlOutput)

	kubeParams := []string{
		"patch",
		"--local",
		"--output=yaml",
		"--patch=" + patchString,
		"--filename=" + filePath,
	}
	err := command.RunExecutable("kubectl", kubeParams...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to apply kubectl command")
	}
	return kubectlOutput.Bytes(), nil
}

func runHelmCommand(runner gitopsUpdateDeploymentExecRunner, config *gitopsUpdateDeploymentOptions, workingDirectory string) ([]byte, error) {
	chartPath, err := piperutils.Files{}.Glob("Chart.yaml")
	dir, _ := filepath.Split(chartPath[0])
	log.Entry().WithError(err).Debug("chartPath[0]=" + chartPath[0])
	additionalValueFile, err := piperutils.Files{}.Glob(config.HelmAdditionalValueFile)
	log.Entry().WithError(err).Debug("additionalValueFile[0]=" + additionalValueFile[0])
	var helmOutput = bytes.Buffer{}
	runner.Stdout(&helmOutput)

	registryImage, err := buildRegistryPlusImageWithoutTag(config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to extract registry URL and image")
	}
	imageTag, err := extractTagFromImageNameTag(config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to extract image tag")
	}
	helmParams := []string{
		"template",
		config.DeploymentName,
		dir,
		"--values=" + filepath.Join(dir, additionalValueFile[0]),
		"--set=" + config.HelmValueForRespositoryAndImageName + "=" + registryImage,
		"--set=" + config.HelmValueForImageVersion + "=" + imageTag,
	}

	err = runner.RunExecutable("helm", helmParams...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to execute helm command")
	}
	return helmOutput.Bytes(), nil
}

func extractTagFromImageNameTag(config *gitopsUpdateDeploymentOptions) (string, error) {
	imageTag, err := docker.ContainerImageTagFromImage(config.ContainerImageNameTag)
	if err != nil {
		return "", errors.Wrap(err, "image name could not be extracted")
	}
	return imageTag, nil
}

func buildRegistryPlusImage(config *gitopsUpdateDeploymentOptions) (string, error) {
	registryURL := config.ContainerRegistryURL
	if registryURL == "" {
		return config.ContainerImageNameTag, nil
	}

	url, err := docker.ContainerRegistryFromURL(registryURL)
	if err != nil {
		return "", errors.Wrap(err, "registry URL could not be extracted")
	}
	if url != "" {
		url = url + "/"
	}
	return url + config.ContainerImageNameTag, nil
}

func buildRegistryPlusImageWithoutTag(config *gitopsUpdateDeploymentOptions) (string, error) {
	registryURL := config.ContainerRegistryURL
	url := ""
	if registryURL != "" {
		containerURL, err := docker.ContainerRegistryFromURL(registryURL)
		if err != nil {
			return "", errors.Wrap(err, "registry URL could not be extracted")
		}
		if containerURL != "" {
			containerURL = containerURL + "/"
		}
		url = containerURL
	}
	imageName, err := docker.ContainerImageNameFromImage(config.ContainerImageNameTag)
	if err != nil {
		return "", errors.Wrap(err, "image name could not be extracted")
	}
	return url + imageName, nil
}

func commitAndPushChanges(config *gitopsUpdateDeploymentOptions, gitUtils iGitopsUpdateDeploymentGitUtils) (plumbing.Hash, error) {
	commit, err := gitUtils.CommitSingleFile(config.FilePath, config.CommitMessage, config.Username)
	if err != nil {
		return [20]byte{}, errors.Wrap(err, "committing changes failed")
	}

	err = gitUtils.PushChangesToRepository(config.Username, config.Password)
	if err != nil {
		return [20]byte{}, errors.Wrap(err, "pushing changes failed")
	}

	return commit, nil
}
