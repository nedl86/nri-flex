package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"nri-flex/internal/formatter"
	"nri-flex/internal/load"
	"nri-flex/internal/logger"
	"nri-flex/internal/processor"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/docker/docker/api"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/newrelic/infra-integrations-sdk/log"
)

// Run discover containers
func Run(containerDiscoveryAvailable *bool, containers *[]types.Container) {
	cli, err := setDockerClient()
	if err != nil {
		logger.Flex("debug", err, "unable to set docker client", false)
	} else {
		ctx := context.Background()
		containerList, err := cli.ContainerList(ctx, types.ContainerListOptions{})
		if err != nil {
			logger.Flex("debug", err, "unable to set perform container list", false)
		} else if len(containerList) > 0 {
			*containers = containerList
			*containerDiscoveryAvailable = true

		}
	}
}

// setDockerClient - Required as there can be edge cases when the integration API version, may need a matching or lower API version then the hosts docker API version
func setDockerClient() (*client.Client, error) {
	var out []byte
	var cli *client.Client
	var err error

	if load.Args.DockerAPIVersion != "" {
		cli, err = client.NewClientWithOpts(client.WithVersion(load.Args.DockerAPIVersion))
	} else {
		log.Debug("GOOS:", runtime.GOOS)

		if err != nil {
			if runtime.GOOS == "windows" {
				out, err = exec.Command("cmd", "/C", `docker`, `version`, `--format`, `"{{json .Client.APIVersion}}"`).Output()
			} else {
				out, err = exec.Command(`docker`, `version`, `--format`, `"{{json .Client.APIVersion}}"`).Output()
				if err != nil {
					out, err = exec.Command(`/host/usr/local/bin/docker`, `version`, `--format`, `"{{json .Client.APIVersion}}"`).Output()
				}
			}
		}

		if err != nil {
			log.Debug("Unable to fetch Docker API version", err)
			log.Debug("Setting client with NewClientWithOpts()")
			cli, err = client.NewClientWithOpts()
		} else {
			cmdOut := string(out)
			clientAPIVersion := strings.TrimSpace(strings.Replace(cmdOut, `"`, "", -1))
			clientVer, _ := strconv.ParseFloat(clientAPIVersion, 64)
			apiVer, _ := strconv.ParseFloat(api.DefaultVersion, 64)

			if clientVer <= apiVer {
				log.Debug("Setting client with version:", clientAPIVersion)
				cli, err = client.NewClientWithOpts(client.WithVersion(clientAPIVersion))
			} else {
				log.Debug("Client API Version", clientAPIVersion, "is higher then integration version", api.DefaultVersion)
				log.Debug("Setting client with NewClientWithOpts()")
				cli, err = client.NewClientWithOpts()
			}
		}
	}

	return cli, err
}

// CreateDynamicContainerConfigs Creates dynamic configs for each container
func CreateDynamicContainerConfigs(containers []types.Container, files []os.FileInfo, path string, ymls *[]load.Config) {
	var containerYmls []load.Config
	processor.LoadConfigFiles(&containerYmls, files, path)
	foundTargetContainerIds := []string{}

	cli, err := setDockerClient()
	if err != nil {
		logger.Flex("debug", err, "unable to set docker client", false)
	} else {
		// store inspected containers, so we do not re-inspect anything unnecessarily
		inspectedContainers := []types.ContainerJSON{}
		logger.Flex("debug", fmt.Errorf("containers %d, containerDiscoveryYmls %d", len(containers), len(containerYmls)), "", false)
		// find flex container id if not set
		findFlexContainerID(&containers)
		// flex envs/labels -> container
		runReverseLookup(cli, &containers, &inspectedContainers, &foundTargetContainerIds, &containerYmls, ymls, path)
		// container envs/labels -> flex
		runForwardLookup(cli, &containers, &inspectedContainers, &foundTargetContainerIds, &containerYmls, ymls, path)
	}
}

// runForwardLookup container envs -> flex
func runForwardLookup(dockerClient *client.Client, containers *[]types.Container, inspectedContainers *[]types.ContainerJSON, foundTargetContainerIds *[]string, containerYmls *[]load.Config, ymls *[]load.Config, path string) {
	discoveryConfigs := map[string]map[string]interface{}{}

	var wg sync.WaitGroup
	wg.Add(len(*containers))
	for _, container := range *containers {
		go func(container types.Container) {
			defer wg.Done()

			// do not target already targeted containers
			targeted := false
			for _, foundTargetContainerID := range *foundTargetContainerIds {
				if foundTargetContainerID == container.ID {
					targeted = true
				}
			}

			// do not do a forward lookup against the flex container
			if load.ContainerID != container.ID && !targeted {

				discoveryLoop := map[string]string{}
				// add container labels to check for disc configs
				for key, val := range container.Labels {
					discoveryLoop[key] = val
				}

				// check env variables for disc configs, so use container inspect
				var containerInspect types.ContainerJSON

				// check if the container has already been inspected
				for _, inspectedContainer := range *inspectedContainers {
					if inspectedContainer.ID == container.ID {
						containerInspect = inspectedContainer
						break
					}
				}

				if containerInspect.Config == nil {
					ctx := context.Background()
					var err error
					containerInspect, err = dockerClient.ContainerInspect(ctx, container.ID)
					if err != nil {
						logger.Flex("debug", nil, "container inspect failed", false)
					} else {
						*inspectedContainers = append(*inspectedContainers, containerInspect)
					}
				}

				if containerInspect.Config != nil {
					// add container labels to check for disc configs
					for key, val := range containerInspect.Config.Labels {
						discoveryLoop[key] = val
					}

					// add env variables to check for disc configs
					for _, envVar := range containerInspect.Config.Env {
						environmentVar := strings.SplitN(envVar, "=", 2)
						if len(environmentVar) == 2 {
							discoveryLoop[environmentVar[0]] = environmentVar[1]
						}
					}

					// create discoveryConfigs - look for flex label and split
					for key, val := range discoveryLoop {
						if strings.Contains(key, "flexDiscovery") {
							logger.Flex("debug", fmt.Errorf("fwd lookup for %v", key), "", false)
							discoveryConfigs[key] = map[string]interface{}{}
							parseFlexDiscoveryLabel(&discoveryConfigs, key, val)
							// t = target, c = config, r = reverse, tt = target type, tm = target mode, ip = ip mode, p = port
							// check if we have a target to find, and config to run
							if discoveryConfigs[key]["t"] != nil {
								// if config is nil, use the <target> , as the yaml file to look up eg. if target (t) = redis, lookup the config (c) redis.yml
								if discoveryConfigs[key]["c"] == nil {
									discoveryConfigs[key]["c"] = discoveryConfigs[key]["t"]
								}
								// auto will mean that if set to true, it will loop through all other containers to find a match
								// if not set / set to false it will target the current container
								if discoveryConfigs[key]["r"] == nil {
									discoveryConfigs[key]["r"] = "false"
								}
								if discoveryConfigs[key]["tt"] == nil {
									discoveryConfigs[key]["tt"] = load.Img // cname == containerName , img = image
								}
								if discoveryConfigs[key]["tm"] == nil {
									discoveryConfigs[key]["tm"] = "contains"
								}

								// addDynamicConfig will ensure the config file matches, so the above parameters are no longer enforced
								addDynamicConfig(containerYmls, &discoveryConfigs, ymls, container, containerInspect, key, path)
							}
						}
					}
				}
			}
		}(container)
	}
	wg.Wait()
}

// runReverseLookup flex envs -> container
func runReverseLookup(dockerClient *client.Client, containers *[]types.Container, inspectedContainers *[]types.ContainerJSON, foundTargetContainerIds *[]string, containerYmls *[]load.Config, ymls *[]load.Config, path string) {
	var flexContainerInspect types.ContainerJSON
	ctx := context.Background()
	discoveryLoop := map[string]string{}
	discoveryConfigs := map[string]map[string]interface{}{}

	if load.ContainerID != "" {
		var err error
		flexContainerInspect, err = dockerClient.ContainerInspect(ctx, load.ContainerID)
		if err != nil {
			logger.Flex("debug", nil, "container inspect failed", false)
		} else if flexContainerInspect.Config != nil {
			*inspectedContainers = append(*inspectedContainers, flexContainerInspect)

			// add container labels to check for disc configs
			for key, val := range flexContainerInspect.Config.Labels {
				discoveryLoop[key] = val
			}

			// add env variables to check for disc configs
			for _, envVar := range flexContainerInspect.Config.Env {
				environmentVar := strings.SplitN(envVar, "=", 2)
				if len(environmentVar) == 2 {
					discoveryLoop[environmentVar[0]] = environmentVar[1]
				}
			}
		}
	}

	// check our discovery loop continue reverse lookup
	if len(discoveryLoop) > 0 {
		var wg sync.WaitGroup
		wg.Add(len(*containers))
		for _, container := range *containers {
			go func(container types.Container) {
				defer wg.Done()
				// do not target the flex container
				if container.ID != load.ContainerID {
					// create discoveryConfigs - look for flex label and split
					for key, val := range discoveryLoop {
						if strings.Contains(key, "flexDiscovery") {
							discoveryConfigs[key] = map[string]interface{}{}
							parseFlexDiscoveryLabel(&discoveryConfigs, key, val)
							// t = target, c = config, r = reverse, tt = target type, tm = target mode, ip = ip mode, p = port
							// check if we have a target to find, and config to run
							if discoveryConfigs[key]["t"] != nil {
								// if config is nil, use the <target> , as the yaml file to look up eg. if target (t) = redis, lookup the config (c) redis.yml
								if discoveryConfigs[key]["c"] == nil {
									discoveryConfigs[key]["c"] = discoveryConfigs[key]["t"]
								}
								if discoveryConfigs[key]["tt"] == nil {
									discoveryConfigs[key]["tt"] = load.Img // cname == containerName , img = image
								}
								if discoveryConfigs[key]["tm"] == nil {
									discoveryConfigs[key]["tm"] = "contains"
								}
							}
							ctx := context.Background()
							reverseContainerInspect, err := dockerClient.ContainerInspect(ctx, container.ID)
							if err != nil {
								logger.Flex("debug", fmt.Errorf("rev container inspect failed on cid:%v key:%v val:%v", container.ID, key, val), "", false)
							} else {
								if findContainerTarget(&discoveryConfigs, container, key, foundTargetContainerIds) {
									logger.Flex("debug", fmt.Errorf("rev lookup matched %v: %v - %v", container.ID, key, val), "", false)
									*inspectedContainers = append(*inspectedContainers, reverseContainerInspect)
									addDynamicConfig(containerYmls, &discoveryConfigs, ymls, container, reverseContainerInspect, key, path)
								}
							}
						}
					}
				}
			}(container)
		}
		wg.Wait()
	}

}

func addDynamicConfig(containerYmls *[]load.Config, discoveryConfigs *map[string]map[string]interface{}, ymls *[]load.Config, targetContainer types.Container, targetContainerInspect types.ContainerJSON, key string, path string) {
	for _, containerYml := range *containerYmls {
		configName := ""
		switch cfg := (*discoveryConfigs)[key]["c"].(type) {
		case string:
			configName = cfg + ".yml"
		default:
			logger.Flex("debug", fmt.Errorf("container discovery config file error %v", ((*discoveryConfigs)[key]["c"])), "", false)
		}
		if containerYml.FileName == configName {
			logger.Flex("debug", fmt.Errorf("container discovery %v matched %v", targetContainer.ID, containerYml.FileName), "", false)
			b, err := ioutil.ReadFile(path + containerYml.FileName)
			if err != nil {
				logger.Flex("debug", err, "unable to read flex config: "+path+containerYml.FileName, false)
			} else {
				ymlString := string(b)
				discoveryIPAddress := "" // we require IP at least
				discoveryPort := ""      // we don't require port
				networkIPAddress := ""
				privatePort := ""
				publicIPAddress := ""
				publicPort := ""
				// use the first found IPAddress
				for _, network := range targetContainer.NetworkSettings.Networks {
					networkIPAddress = network.IPAddress
					break
				}
				// ability to override and select port
				if (*discoveryConfigs)[key]["p"] != nil {
					discoveryPort = (*discoveryConfigs)[key]["p"].(string)
				} else {
					// use the first found public port
					for _, port := range targetContainer.Ports {
						publicIPAddress = fmt.Sprintf("%v", port.IP)
						publicPort = fmt.Sprintf("%v", port.PublicPort)
						privatePort = fmt.Sprintf("%v", port.PrivatePort)
						break
					}
				}

				ipMode := load.DefaultIPMode
				if load.Args.OverrideIPMode != "" && (load.Args.OverrideIPMode == load.Public || load.Args.OverrideIPMode == load.Private) {
					ipMode = load.Args.OverrideIPMode
				} else if (*discoveryConfigs)[key]["ip"] != nil {
					if (*discoveryConfigs)[key]["ip"].(string) == load.Private || (*discoveryConfigs)[key]["ip"].(string) == load.Public {
						ipMode = (*discoveryConfigs)[key]["ip"].(string)
					}
				}

				switch ipMode {
				case load.Public:
					discoveryIPAddress = publicIPAddress
					discoveryPort = publicPort
				case load.Private:
					discoveryIPAddress = networkIPAddress
					discoveryPort = privatePort
				}

				// attempt low level ip fetch
				lowLevelIpv4Fetch(&discoveryIPAddress, targetContainerInspect.State.Pid)
				// attempt hostname fallback
				execHostnameFallback(&discoveryIPAddress, targetContainer.ID)

				if discoveryIPAddress != "" {
					// substitute ip into yml
					ymlString = strings.Replace(ymlString, "${auto:host}", discoveryIPAddress, -1)
					ymlString = strings.Replace(ymlString, "${auto:ip}", discoveryIPAddress, -1)
				}

				if discoveryPort != "" {
					// substitute port into yml
					ymlString = strings.Replace(ymlString, "${auto:port}", discoveryPort, -1)
				} else {
					// kubernetes port fallback
					for key, val := range targetContainer.Labels {
						if key == "annotation.io.kubernetes.container.ports" {
							var x []interface{}
							err := json.Unmarshal([]byte(val), &x)
							if err == nil {
								for _, kubePort := range x {
									if kubePort.(map[string]interface{})["containerPort"] != nil {
										discoveryPort = fmt.Sprintf("%v", kubePort.(map[string]interface{})["containerPort"])
										ymlString = strings.Replace(ymlString, "${auto:port}", discoveryPort, -1)
										break
									}
								}
							}
						}
					}
					// secondary inspect fallback
					if discoveryPort == "" {
						if targetContainerInspect.Config != nil {
							for port := range targetContainerInspect.Config.ExposedPorts {
								discoveryPort = strings.Split(port.Port(), "/")[0]
								ymlString = strings.Replace(ymlString, "${auto:port}", discoveryPort, -1)
								break
							}
						}
					}
				}

				if strings.Contains(ymlString, "${auto:host}") || strings.Contains(ymlString, "${auto:ip}") || strings.Contains(ymlString, "${auto:port}") {
					logger.Flex("debug", nil, "couldn't build dynamic cfg for: "+targetContainer.Image+" - "+targetContainer.ID, false)
					logger.Flex("debug", nil, "missing variable unable to create dynamic cfg ip:<"+discoveryIPAddress+">-port:<"+discoveryPort+">", false)
				} else {
					yml, err := processor.ReadYML(ymlString)
					if err != nil {
						logger.Flex("debug", err, "unable to unmarshal yml config: "+path+containerYml.FileName, false)
						logger.Flex("debug", fmt.Errorf(ymlString), "", false)
					} else {
						if yml.CustomAttributes == nil {
							yml.CustomAttributes = map[string]string{}
						}
						for key, val := range targetContainer.Labels {
							yml.CustomAttributes[key] = val
						}
						yml.CustomAttributes["containerID"] = targetContainer.ID
						yml.CustomAttributes["image"] = targetContainer.Image
						yml.CustomAttributes["IDShort"] = targetContainer.ID[0:12]
						*ymls = append(*ymls, yml)
					}
				}

			}

		} else {
			logger.Flex("debug", fmt.Errorf("container discovery %v : containerFileName %v did not match configName %v", targetContainer.ID, containerYml.FileName, configName), "", false)
		}
	}
}

func parseFlexDiscoveryLabel(discoveryConfigs *map[string]map[string]interface{}, key string, val string) {
	if strings.Contains(val, "=") { // nicer for other setups
		labelValues := strings.Split(val, ",")
		for _, value := range labelValues {
			configKeyPair := strings.Split(value, "=")
			if len(configKeyPair) == 2 {
				(*discoveryConfigs)[key][configKeyPair[0]] = configKeyPair[1]
			}
		}
	} else if strings.Contains(val, ".") { // needed for kubernetes eg. flexDiscoveryRedis:"t_redis.c_redis.tt_img.tm_contains"
		labelValues := strings.Split(val, ".")
		for _, value := range labelValues {
			configKeyPair := strings.Split(value, "_")
			if len(configKeyPair) == 2 {
				(*discoveryConfigs)[key][configKeyPair[0]] = configKeyPair[1]
			}
		}
	}
}

func findContainerTarget(discoveryConfigs *map[string]map[string]interface{}, container types.Container, key string, foundTargetContainerIds *[]string) bool {

	// do not add any dynamic configs for already targeted containers
	for _, id := range *foundTargetContainerIds {
		if id == container.ID {
			return false
		}
	}
	switch (*discoveryConfigs)[key]["tt"].(type) {
	case string:
		switch (*discoveryConfigs)[key]["tt"].(string) {
		case "cname":
			for _, containerName := range container.Names {
				checkContainerName := strings.TrimPrefix(containerName, "/") // docker adds a / in front
				if formatter.KvFinder((*discoveryConfigs)[key]["tm"].(string), checkContainerName, (*discoveryConfigs)[key]["t"].(string)) {
					*(foundTargetContainerIds) = append(*(foundTargetContainerIds), container.ID)
					return true
				}
				// kubernetes container name fallback via label
				for key, val := range container.Labels {
					if key == "io.kubernetes.container.name" {
						if formatter.KvFinder((*discoveryConfigs)[key]["tm"].(string), val, (*discoveryConfigs)[key]["t"].(string)) {
							*(foundTargetContainerIds) = append(*(foundTargetContainerIds), container.ID)
							return true
						}
					}
				}
			}
		case load.Img:
			if formatter.KvFinder((*discoveryConfigs)[key]["tm"].(string), container.Image, (*discoveryConfigs)[key]["t"].(string)) {
				*(foundTargetContainerIds) = append(*(foundTargetContainerIds), container.ID)
				return true
			}
		}
	case nil:
		logger.Flex("debug", nil, "targetType not set id: "+container.ID, false)
	}

	return false
}

func lowLevelIpv4Fetch(discoveryIPAddress *string, pid int) {
	if *discoveryIPAddress == "" {
		// targetContainerInspect.State.Pid
		// cat /host/proc/<pid>/net/fib_trie | awk '/32 host/ { print f } {f=$2}' | grep -v 127.0.0.1 | sort -u

		logger.Flex("info", nil, "attempting low level ip fetch", false)

		// Create a new context and add a timeout to it
		ctx, cancel := context.WithTimeout(context.Background(), load.DefaultTimeout)
		defer cancel() // The cancel should be deferred so resources are cleaned up

		// Create the command with our context
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `cat /host/proc/`+fmt.Sprintf("%v", pid)+
			`/net/fib_trie | awk '/32 host/ { print f } {f=$2}' | grep -v 127.0.0.1 | sort -u`)
		output, err := cmd.CombinedOutput()

		if err != nil {
			message := "command failed: " + ""
			if output != nil {
				message = message + " " + string(output)
			}
			logger.Flex("debug", err, message, false)
		} else if ctx.Err() == context.DeadlineExceeded {
			logger.Flex("debug", ctx.Err(), "command timed out", false)
		} else if ctx.Err() != nil {
			logger.Flex("debug", err, "command execution failed", false)
		} else {
			ipv4 := strings.TrimSpace(string(output))
			// ensure this is an ipv4 address
			re := regexp.MustCompile(`\b((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)(\.|$)){4}\b`)
			if re.Match([]byte(ipv4)) {
				logger.Flex("info", nil, fmt.Sprintf("fetched %v", ipv4), false)
				*discoveryIPAddress = ipv4
			} else {
				logger.Flex("debug", fmt.Errorf("low level fetch failed %v", ipv4), "", false)
			}
		}
	}
}

func execHostnameFallback(discoveryIPAddress *string, containerID string) {
	if *discoveryIPAddress == "" {
		// fall back if IP is not discovered
		// attempt to directly fetch IP from container
		ip, err := execContainerCommand(containerID, []string{"hostname", "-i"})
		ipv4 := strings.TrimSpace(ip)
		re := regexp.MustCompile(`\b((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)(\.|$)){4}\b`)
		if err != nil {
			logger.Flex("debug", err, "secondary fetch container ip failed", false)
		} else if ip != "" && re.Match([]byte(ipv4)) && !strings.Contains(ip, "exec failed") {
			*discoveryIPAddress = ipv4
		}
	}
}

func findFlexContainerID(containers *[]types.Container) {
	// fallback on looking for image name "nri-flex" if flex's container id was not found internally
	if load.ContainerID == "" {
		logger.Flex("debug", fmt.Errorf("flex container id has not been found internally"), "", false)
		logger.Flex("debug", fmt.Errorf("falling back - looking for 'nri-flex' image or container name"), "", false)

		var wg sync.WaitGroup
		wg.Add(len(*containers))
		for _, container := range *containers {
			go func(container types.Container) {
				defer wg.Done()
				if strings.Contains(container.Image, load.IntegrationName) && load.ContainerID == "" {
					load.ContainerID = container.ID
				}

				// fallback - check standard container names
				if load.ContainerID == "" {
					for _, containerName := range container.Names {
						checkContainerName := strings.TrimPrefix(containerName, "/") // docker adds a / in front
						if strings.Contains(checkContainerName, load.IntegrationNameShort) {
							load.ContainerID = container.ID
						}
					}
				}

				// fallback - check kubernetes container name via label
				if load.ContainerID == "" {
					for key, val := range container.Labels {
						if key == "io.kubernetes.container.name" {
							if strings.Contains(val, load.IntegrationNameShort) {
								load.ContainerID = container.ID
							}
						}
					}
				}

			}(container)
		}
		wg.Wait()

		if load.ContainerID == "" {
			logger.Flex("debug", fmt.Errorf("unable to find flex container id"), "", false)
		} else {
			logger.Flex("debug", fmt.Errorf("flex container id: %v", load.ContainerID), "", false)
		}
	}
}