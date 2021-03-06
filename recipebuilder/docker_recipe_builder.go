package recipebuilder

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/bbs/models"
	ssh_routes "github.com/cloudfoundry-incubator/diego-ssh/routes"
	"github.com/cloudfoundry-incubator/nsync/helpers"
	"github.com/cloudfoundry-incubator/runtime-schema/cc_messages"
	"github.com/pivotal-golang/lager"
)

const (
	DockerScheme      = "docker"
	DockerIndexServer = "docker.io"
)

type DockerRecipeBuilder struct {
	logger lager.Logger
	config Config
}

func NewDockerRecipeBuilder(logger lager.Logger, config Config) *DockerRecipeBuilder {
	return &DockerRecipeBuilder{
		logger: logger,
		config: config,
	}
}

func (b *DockerRecipeBuilder) Build(desiredApp *cc_messages.DesireAppRequestFromCC) (*models.DesiredLRP, error) {
	lrpGuid := desiredApp.ProcessGuid

	buildLogger := b.logger.Session("message-builder")

	if desiredApp.DockerImageUrl == "" {
		buildLogger.Error("desired-app-invalid", ErrDockerImageMissing, lager.Data{"desired-app": desiredApp})
		return nil, ErrDockerImageMissing
	}

	if desiredApp.DropletUri != "" && desiredApp.DockerImageUrl != "" {
		buildLogger.Error("desired-app-invalid", ErrMultipleAppSources, lager.Data{"desired-app": desiredApp})
		return nil, ErrMultipleAppSources
	}

	if desiredApp.DockerImageUrl == "" {
		return nil, ErrNoDockerImage
	}

	var lifecycle = "docker"
	lifecyclePath, ok := b.config.Lifecycles[lifecycle]
	if !ok {
		buildLogger.Error("unknown-lifecycle", ErrNoLifecycleDefined, lager.Data{
			"lifecycle": lifecycle,
		})

		return nil, ErrNoLifecycleDefined
	}

	lifecycleURL := lifecycleDownloadURL(lifecyclePath, b.config.FileServerURL)

	rootFSPath := ""
	var err error
	rootFSPath, err = convertDockerURI(desiredApp.DockerImageUrl)
	if err != nil {
		return nil, err
	}

	var privilegedContainer bool
	var containerEnvVars []*models.EnvironmentVariable

	numFiles := DefaultFileDescriptorLimit
	if desiredApp.FileDescriptors != 0 {
		numFiles = desiredApp.FileDescriptors
	}

	var setup []models.ActionInterface
	var actions []models.ActionInterface
	var monitor models.ActionInterface

	executionMetadata, err := NewDockerExecutionMetadata(desiredApp.ExecutionMetadata)
	if err != nil {
		b.logger.Error("parsing-execution-metadata-failed", err, lager.Data{
			"desired-app-metadata": executionMetadata,
		})
		return nil, err
	}

	user, err := extractUser(executionMetadata)
	if err != nil {
		return nil, err
	}

	setup = append(setup, &models.DownloadAction{
		From:     lifecycleURL,
		To:       "/tmp/lifecycle",
		CacheKey: fmt.Sprintf("%s-lifecycle", strings.Replace(lifecycle, "/", "-", 1)),
		User:     user,
	})

	desiredAppPorts, err := extractExposedPorts(executionMetadata, b.logger)
	if err != nil {
		return nil, err
	}

	switch desiredApp.HealthCheckType {
	case cc_messages.PortHealthCheckType, cc_messages.UnspecifiedHealthCheckType:
		monitor = models.Timeout(getParallelAction(desiredAppPorts, user), 30*time.Second)
	}

	actions = append(actions, &models.RunAction{
		User: user,
		Path: "/tmp/lifecycle/launcher",
		Args: append(
			[]string{"app"},
			desiredApp.StartCommand,
			desiredApp.ExecutionMetadata,
		),
		Env:       createLrpEnv(desiredApp.Environment, desiredAppPorts[0]),
		LogSource: AppLogSource,
		ResourceLimits: &models.ResourceLimits{
			Nofile: &numFiles,
		},
	})

	desiredAppRoutingInfo, err := helpers.CCRouteInfoToRoutes(desiredApp.RoutingInfo, desiredAppPorts)
	if err != nil {
		buildLogger.Error("marshaling-cc-route-info-failed", err)
		return nil, err
	}

	if desiredApp.AllowSSH {
		hostKeyPair, err := b.config.KeyFactory.NewKeyPair(1024)
		if err != nil {
			buildLogger.Error("new-host-key-pair-failed", err)
			return nil, err
		}

		userKeyPair, err := b.config.KeyFactory.NewKeyPair(1024)
		if err != nil {
			buildLogger.Error("new-user-key-pair-failed", err)
			return nil, err
		}

		actions = append(actions, &models.RunAction{
			User: user,
			Path: "/tmp/lifecycle/diego-sshd",
			Args: []string{
				"-address=" + fmt.Sprintf("0.0.0.0:%d", DefaultSSHPort),
				"-hostKey=" + hostKeyPair.PEMEncodedPrivateKey(),
				"-authorizedKey=" + userKeyPair.AuthorizedKey(),
				"-inheritDaemonEnv",
				"-logLevel=fatal",
			},
			Env: createLrpEnv(desiredApp.Environment, desiredAppPorts[0]),
			ResourceLimits: &models.ResourceLimits{
				Nofile: &numFiles,
			},
		})

		sshRoutePayload, err := json.Marshal(ssh_routes.SSHRoute{
			ContainerPort:   2222,
			PrivateKey:      userKeyPair.PEMEncodedPrivateKey(),
			HostFingerprint: hostKeyPair.Fingerprint(),
		})

		if err != nil {
			buildLogger.Error("marshaling-ssh-route-failed", err)
			return nil, err
		}

		sshRouteMessage := json.RawMessage(sshRoutePayload)
		desiredAppRoutingInfo[ssh_routes.DIEGO_SSH] = &sshRouteMessage
		desiredAppPorts = append(desiredAppPorts, DefaultSSHPort)
	}

	setupAction := models.Serial(setup...)
	actionAction := models.Codependent(actions...)

	return &models.DesiredLRP{
		Privileged: privilegedContainer,

		Domain: cc_messages.AppLRPDomain,

		ProcessGuid: lrpGuid,
		Instances:   int32(desiredApp.NumInstances),
		Routes:      &desiredAppRoutingInfo,
		Annotation:  desiredApp.ETag,

		CpuWeight: cpuWeight(desiredApp.MemoryMB),

		MemoryMb: int32(desiredApp.MemoryMB),
		DiskMb:   int32(desiredApp.DiskMB),

		Ports: desiredAppPorts,

		RootFs: rootFSPath,

		LogGuid:   desiredApp.LogGuid,
		LogSource: LRPLogSource,

		MetricsGuid: desiredApp.LogGuid,

		EnvironmentVariables: containerEnvVars,
		Setup:                models.WrapAction(setupAction),
		Action:               models.WrapAction(actionAction),
		Monitor:              models.WrapAction(monitor),

		StartTimeout: uint32(desiredApp.HealthCheckTimeoutInSeconds),

		EgressRules: desiredApp.EgressRules,
	}, nil
}

func (b DockerRecipeBuilder) ExtractExposedPorts(desiredApp *cc_messages.DesireAppRequestFromCC) ([]uint32, error) {
	executionMetadata := desiredApp.ExecutionMetadata
	metadata, err := NewDockerExecutionMetadata(executionMetadata)
	if err != nil {
		return nil, err
	}
	return extractExposedPorts(metadata, b.logger)
}

func extractExposedPorts(executionMetadata DockerExecutionMetadata, logger lager.Logger) ([]uint32, error) {
	var exposedPort uint32 = DefaultPort
	exposedPorts := executionMetadata.ExposedPorts
	ports := make([]uint32, 0)
	if len(exposedPorts) == 0 {
		ports = append(ports, exposedPort)
	}
	for _, port := range exposedPorts {
		if port.Protocol == "tcp" {
			exposedPort = port.Port
			ports = append(ports, exposedPort)
		}
	}

	if len(ports) == 0 {
		err := fmt.Errorf("No tcp ports found in image metadata")
		logger.Error("parsing-exposed-ports-failed", err, lager.Data{
			"desired-app-metadata": executionMetadata,
		})
		return nil, err
	}

	return ports, nil
}

func extractUser(executionMetadata DockerExecutionMetadata) (string, error) {
	if len(executionMetadata.User) > 0 {
		return executionMetadata.User, nil
	} else {
		return "root", nil
	}
}

func convertDockerURI(dockerURI string) (string, error) {
	if strings.Contains(dockerURI, "://") {
		return "", errors.New("docker URI [" + dockerURI + "] should not contain scheme")
	}

	indexName, remoteName, tag := parseDockerRepoUrl(dockerURI)

	return (&url.URL{
		Scheme:   DockerScheme,
		Path:     indexName + "/" + remoteName,
		Fragment: tag,
	}).String(), nil
}

// via https://github.com/docker/docker/blob/a271eaeba224652e3a12af0287afbae6f82a9333/registry/config.go#L295
func parseDockerRepoUrl(dockerURI string) (indexName, remoteName, tag string) {
	nameParts := strings.SplitN(dockerURI, "/", 2)

	if officialRegistry(nameParts) {
		// URI without host
		indexName = ""
		remoteName = dockerURI

		// URI has format docker.io/<path>
		if nameParts[0] == DockerIndexServer {
			indexName = DockerIndexServer
			remoteName = nameParts[1]
		}

		// Remote name contain no '/' - prefix it with "library/"
		// via https://github.com/docker/docker/blob/a271eaeba224652e3a12af0287afbae6f82a9333/registry/config.go#L343
		if strings.IndexRune(remoteName, '/') == -1 {
			remoteName = "library/" + remoteName
		}
	} else {
		indexName = nameParts[0]
		remoteName = nameParts[1]
	}

	remoteName, tag = parseDockerRepositoryTag(remoteName)

	return indexName, remoteName, tag
}

func officialRegistry(nameParts []string) bool {
	return len(nameParts) == 1 ||
		nameParts[0] == DockerIndexServer ||
		(!strings.Contains(nameParts[0], ".") &&
			!strings.Contains(nameParts[0], ":") &&
			nameParts[0] != "localhost")
}

// via https://github.com/docker/docker/blob/4398108/pkg/parsers/parsers.go#L72
func parseDockerRepositoryTag(remoteName string) (string, string) {
	n := strings.LastIndex(remoteName, ":")
	if n < 0 {
		return remoteName, ""
	}
	if tag := remoteName[n+1:]; !strings.Contains(tag, "/") {
		return remoteName[:n], tag
	}
	return remoteName, ""
}
