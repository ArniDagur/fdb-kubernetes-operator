/*
 * host_network_test.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2026 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package internal

import (
	"encoding/json"
	"strings"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/v2/api/v1beta2"
	monitorapi "github.com/apple/foundationdb/fdbkubernetesmonitor/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// mainContainer returns the main container of the pod spec.
func mainContainer(spec *corev1.PodSpec) corev1.Container {
	for _, container := range spec.Containers {
		if container.Name == fdbv1beta2.MainContainerName {
			return container
		}
	}

	Fail("the pod spec has no main container")
	return corev1.Container{}
}

// configMapKey returns the ConfigMap key that the pod spec mounts as the monitor configuration.
func configMapKey(spec *corev1.PodSpec) string {
	for _, volume := range spec.Volumes {
		if volume.Name != "config-map" || volume.ConfigMap == nil {
			continue
		}

		for _, item := range volume.ConfigMap.Items {
			if item.Path == "config.json" {
				return item.Key
			}
		}
	}

	return ""
}

var _ = Describe("host networking", func() {
	var cluster *fdbv1beta2.FoundationDBCluster

	BeforeEach(func() {
		cluster = CreateDefaultCluster()
		cluster.Spec.ImageType = ptr.To(fdbv1beta2.ImageTypeUnified)
		cluster.Status.ImageTypes = []fdbv1beta2.ImageType{fdbv1beta2.ImageTypeUnified}
		cluster.Status.RequiredAddresses = fdbv1beta2.RequiredAddressSet{TLS: true}
		cluster.Status.ConnectionString = "operator-test:asdfasf@127.0.0.1:4501"
		cluster.Spec.StorageServersPerPod = 2
		cluster.Spec.Routing.HostNetwork = &fdbv1beta2.HostNetworkConfig{
			Enabled:        ptr.To(true),
			PortRangeStart: ptr.To(20000),
			PortRangeEnd:   ptr.To(20999),
		}
		Expect(NormalizeClusterSpec(cluster, DeprecationOptions{})).To(Succeed())
	})

	When("generating the pod spec", func() {
		var processGroup *fdbv1beta2.ProcessGroupStatus
		var spec *corev1.PodSpec

		BeforeEach(func() {
			processGroup = GetProcessGroup(cluster, fdbv1beta2.ProcessClassStorage, 1)
			// A block for two storage servers: 20100/20101, 20102/20103, and the metrics port 20104.
			processGroup.PortBlock = &fdbv1beta2.PortBlock{Start: 20100, ServersPerPod: 2}
		})

		JustBeforeEach(func() {
			var err error
			spec, err = GetPodSpec(cluster, processGroup)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should run the pod in the host network namespace with cluster DNS", func() {
			Expect(spec.HostNetwork).To(BeTrue())
			Expect(spec.DNSPolicy).To(Equal(corev1.DNSClusterFirstWithHostNet))
		})

		It("should record the block in the environment of the main container", func() {
			container := mainContainer(spec)
			Expect(container.Env).To(ContainElements(
				corev1.EnvVar{Name: fdbv1beta2.EnvNamePortBlockStart, Value: "20100"},
				corev1.EnvVar{Name: "STORAGE_SERVERS_PER_POD", Value: "2"},
			))
		})

		It("should run the metrics endpoint on the last port of the block", func() {
			Expect(
				strings.Join(mainContainer(spec).Args, " "),
			).To(ContainSubstring("--listen-address :20104"))
		})

		It("should declare every port of the block as a host port", func() {
			ports := map[string]int32{}
			for _, port := range mainContainer(spec).Ports {
				Expect(port.HostPort).To(Equal(port.ContainerPort))
				ports[port.Name] = port.ContainerPort
			}
			Expect(ports).To(Equal(map[string]int32{
				"tls":       20100,
				"non-tls":   20101,
				"tls-2":     20102,
				"non-tls-2": 20103,
				"metrics":   20104,
			}))
		})

		It("should mount the host network monitor configuration", func() {
			Expect(configMapKey(spec)).To(Equal("fdbmonitor-conf-storage-host-network-json"))
		})

		When("the desired servers per pod changed after the block was assigned", func() {
			BeforeEach(func() {
				cluster.Spec.StorageServersPerPod = 3
			})

			It("should keep the number of servers of the block", func() {
				container := mainContainer(spec)
				Expect(strings.Join(container.Args, " ")).To(ContainSubstring("--process-count 2"))
				Expect(container.Env).To(ContainElement(
					corev1.EnvVar{Name: "STORAGE_SERVERS_PER_POD", Value: "2"},
				))
				Expect(container.Ports).To(HaveLen(5))
			})
		})

		When("the pod template sets a DNS policy and ports", func() {
			BeforeEach(func() {
				cluster.Spec.Processes[fdbv1beta2.ProcessClassGeneral] = fdbv1beta2.ProcessSettings{
					PodTemplate: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							DNSPolicy: corev1.DNSDefault,
							Containers: []corev1.Container{{
								Name: fdbv1beta2.MainContainerName,
								Ports: []corev1.ContainerPort{
									{Name: "metrics", ContainerPort: 8081},
									{Name: "debug", ContainerPort: 9999},
								},
							}},
						},
					},
				}
				Expect(NormalizeClusterSpec(cluster, DeprecationOptions{})).To(Succeed())
			})

			It("should keep the DNS policy of the template", func() {
				Expect(spec.DNSPolicy).To(Equal(corev1.DNSDefault))
			})

			It("should replace template ports with the same name and keep the others", func() {
				ports := map[string]int32{}
				for _, port := range mainContainer(spec).Ports {
					ports[port.Name] = port.ContainerPort
				}
				Expect(ports).To(HaveKeyWithValue("metrics", int32(20104)))
				Expect(ports).To(HaveKeyWithValue("debug", int32(9999)))
			})
		})

		When("the process group has no block yet", func() {
			BeforeEach(func() {
				processGroup.PortBlock = nil
			})

			It("should only contain the settings that don't need a block", func() {
				Expect(spec.HostNetwork).To(BeTrue())
				container := mainContainer(spec)
				Expect(container.Ports).To(BeEmpty())
				for _, env := range container.Env {
					Expect(env.Name).NotTo(Equal(fdbv1beta2.EnvNamePortBlockStart))
				}
				Expect(
					strings.Join(container.Args, " "),
				).NotTo(ContainSubstring("--listen-address"))
			})
		})

		When("host networking is disabled but the process group still has a block", func() {
			BeforeEach(func() {
				cluster.Spec.Routing.HostNetwork.Enabled = ptr.To(false)
			})

			It("should create a pod on the pod network", func() {
				Expect(spec.HostNetwork).To(BeFalse())
				Expect(spec.DNSPolicy).To(BeEmpty())
				container := mainContainer(spec)
				Expect(container.Ports).To(BeEmpty())
				for _, env := range container.Env {
					Expect(env.Name).NotTo(Equal(fdbv1beta2.EnvNamePortBlockStart))
				}
				Expect(configMapKey(spec)).To(Equal("fdbmonitor-conf-storage-json"))
			})
		})

		When("host networking is only set in the pod template", func() {
			var withoutTemplate *corev1.PodSpec

			BeforeEach(func() {
				cluster.Spec.Routing.HostNetwork = nil
				processGroup.PortBlock = nil
			})

			JustBeforeEach(func() {
				var err error
				withoutTemplate, err = GetPodSpec(cluster, processGroup)
				Expect(err).NotTo(HaveOccurred())

				cluster.Spec.Processes[fdbv1beta2.ProcessClassGeneral] = fdbv1beta2.ProcessSettings{
					PodTemplate: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{HostNetwork: true}},
				}
				Expect(NormalizeClusterSpec(cluster, DeprecationOptions{})).To(Succeed())
				spec, err = GetPodSpec(cluster, processGroup)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should keep the pod spec as before, with only hostNetwork added", func() {
				Expect(spec.HostNetwork).To(BeTrue())
				spec.HostNetwork = false
				Expect(spec).To(Equal(withoutTemplate))
			})
		})
	})

	When("generating the monitor configuration for a host-networked pod", func() {
		env := map[string]string{
			fdbv1beta2.EnvNamePublicIP:       "10.1.0.7",
			fdbv1beta2.EnvNamePortBlockStart: "20100",
			fdbv1beta2.EnvNameInstanceID:     "storage-1",
			fdbv1beta2.EnvNameMachineID:      "node-1",
			fdbv1beta2.EnvNameZoneID:         "node-1",
			fdbv1beta2.EnvNameDNSName:        "storage-1.test.svc.cluster.local",
		}

		// publicAddress returns the --public_address argument that the fdb-kubernetes-monitor generates.
		publicAddress := func(config monitorapi.ProcessConfiguration, processNumber int) string {
			arguments, err := config.GenerateArguments(processNumber, env)
			Expect(err).NotTo(HaveOccurred())
			for _, argument := range arguments {
				if strings.HasPrefix(argument, "--public_address=") {
					return argument
				}
			}

			Fail("no public address argument")
			return ""
		}

		It("should compute the port of each process from the block", func() {
			config, err := GetMonitorProcessConfigurationWithNetwork(
				cluster, fdbv1beta2.ProcessClassStorage, 2, fdbv1beta2.ImageTypeUnified, nil, true,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(publicAddress(config, 1)).To(Equal("--public_address=[10.1.0.7]:20100:tls"))
			Expect(publicAddress(config, 2)).To(Equal("--public_address=[10.1.0.7]:20102:tls"))
		})

		It("should compute the TLS and the non-TLS port during a TLS migration", func() {
			cluster.Status.RequiredAddresses = fdbv1beta2.RequiredAddressSet{
				TLS:    true,
				NonTLS: true,
			}
			config, err := GetMonitorProcessConfigurationWithNetwork(
				cluster, fdbv1beta2.ProcessClassStorage, 2, fdbv1beta2.ImageTypeUnified, nil, true,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(publicAddress(config, 2)).
				To(Equal("--public_address=[10.1.0.7]:20102:tls,[10.1.0.7]:20103"))
		})

		It("should use the IP family of the pod", func() {
			env[fdbv1beta2.EnvNamePublicIP] = "10.1.0.7,fd00::7"
			DeferCleanup(func() {
				env[fdbv1beta2.EnvNamePublicIP] = "10.1.0.7"
			})

			config, err := GetMonitorProcessConfigurationWithNetwork(
				cluster,
				fdbv1beta2.ProcessClassStorage,
				2,
				fdbv1beta2.ImageTypeUnified,
				ptr.To(6),
				true,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(publicAddress(config, 2)).To(Equal("--public_address=[fd00::7]:20102:tls"))
		})

		It("should use the Sum argument of the fdb-kubernetes-monitor", func() {
			config, err := GetMonitorProcessConfigurationWithNetwork(
				cluster, fdbv1beta2.ProcessClassStorage, 2, fdbv1beta2.ImageTypeUnified, nil, true,
			)
			Expect(err).NotTo(HaveOccurred())
			jsonData, err := json.Marshal(config)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(jsonData)).To(ContainSubstring(`"type":"Sum"`))
		})

		It("should keep the configuration of pod-network pods unchanged", func() {
			config, err := GetMonitorProcessConfigurationWithNetwork(
				cluster, fdbv1beta2.ProcessClassStorage, 2, fdbv1beta2.ImageTypeUnified, nil, false,
			)
			Expect(err).NotTo(HaveOccurred())
			expected, err := GetMonitorProcessConfiguration(
				cluster, fdbv1beta2.ProcessClassStorage, 2, fdbv1beta2.ImageTypeUnified, nil,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(config).To(Equal(expected))
			Expect(publicAddress(config, 2)).To(Equal("--public_address=[10.1.0.7]:4502:tls"))
		})

		It("should build the expected command line from the pod's environment", func() {
			pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: fdbv1beta2.MainContainerName,
				Env: []corev1.EnvVar{
					{Name: fdbv1beta2.EnvNamePortBlockStart, Value: "20100"},
				},
			}}}}

			substitutions := map[string]string{}
			for key, value := range env {
				substitutions[key] = value
			}
			substitutions[fdbv1beta2.EnvNameBinaryDir] = "/usr/bin"

			command, err := GetStartCommandWithSubstitutions(
				cluster,
				fdbv1beta2.ProcessClassStorage,
				substitutions,
				2,
				2,
				fdbv1beta2.ImageTypeUnified,
				pod,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(command).To(ContainSubstring("--public_address=[10.1.0.7]:20102:tls"))
		})
	})

	When("generating the ConfigMap", func() {
		var withoutHostNetwork map[string]string

		BeforeEach(func() {
			withoutHostNetworkCluster := cluster.DeepCopy()
			withoutHostNetworkCluster.Spec.Routing.HostNetwork = nil
			configMap, err := GetConfigMap(withoutHostNetworkCluster)
			Expect(err).NotTo(HaveOccurred())
			withoutHostNetwork = configMap.Data
		})

		It(
			"should add a host network entry per process class and keep the existing entries",
			func() {
				configMap, err := GetConfigMap(cluster)
				Expect(err).NotTo(HaveOccurred())

				for key, value := range withoutHostNetwork {
					Expect(configMap.Data).To(HaveKeyWithValue(key, value))
				}

				jsonData, ok := configMap.Data["fdbmonitor-conf-storage-host-network-json"]
				Expect(ok).To(BeTrue())
				config := monitorapi.ProcessConfiguration{}
				Expect(json.Unmarshal([]byte(jsonData), &config)).To(Succeed())
				expected, err := GetMonitorProcessConfigurationWithNetwork(
					cluster,
					fdbv1beta2.ProcessClassStorage,
					0,
					fdbv1beta2.ImageTypeUnified,
					nil,
					true,
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(config).To(Equal(expected))
			},
		)

		It("should not add host network entries to clusters without host networking", func() {
			for key := range withoutHostNetwork {
				Expect(key).NotTo(ContainSubstring("host-network"))
			}
		})

		It("should keep the host network entries while a process group still has a block", func() {
			cluster.Spec.Routing.HostNetwork.Enabled = ptr.To(false)
			cluster.Status.ProcessGroups = []*fdbv1beta2.ProcessGroupStatus{{
				ProcessGroupID: "storage-1",
				ProcessClass:   fdbv1beta2.ProcessClassStorage,
				PortBlock:      &fdbv1beta2.PortBlock{Start: 20100, ServersPerPod: 2},
			}}

			configMap, err := GetConfigMap(cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(configMap.Data).To(HaveKey("fdbmonitor-conf-storage-host-network-json"))
		})
	})

	When("reading the port block of a pod", func() {
		It("should read the start and the servers per pod from the environment", func() {
			pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: fdbv1beta2.MainContainerName,
				Env: []corev1.EnvVar{
					{Name: fdbv1beta2.EnvNamePortBlockStart, Value: "20100"},
					{Name: "LOG_SERVERS_PER_POD", Value: "3"},
				},
			}}}}

			Expect(HasPortBlock(pod)).To(BeTrue())
			Expect(GetPortBlock(pod, fdbv1beta2.ProcessClassLog)).
				To(Equal(&fdbv1beta2.PortBlock{Start: 20100, ServersPerPod: 3}))
		})

		It("should not find a block on a pod that only sets hostNetwork", func() {
			pod := &corev1.Pod{Spec: corev1.PodSpec{
				HostNetwork: true,
				Containers:  []corev1.Container{{Name: fdbv1beta2.MainContainerName}},
			}}

			Expect(HasPortBlock(pod)).To(BeFalse())
			Expect(GetPortBlock(pod, fdbv1beta2.ProcessClassLog)).To(BeNil())
		})
	})
})
