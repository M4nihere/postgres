//go:build envtest
// +build envtest

/*
 Copyright 2021 - 2023 Crunchy Data Solutions, Inc.
 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package postgrescluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/percona/percona-postgresql-operator/internal/initialize"
	"github.com/percona/percona-postgresql-operator/internal/naming"
	"github.com/percona/percona-postgresql-operator/internal/pgmonitor"
	"github.com/percona/percona-postgresql-operator/internal/testing/require"
	"github.com/percona/percona-postgresql-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

var (
	exporterExtendQueryPathFlag  = "--extend.query-path=/opt/crunchy/conf/queries.yml"
	exporterWebListenAddressFlag = fmt.Sprintf("--web.listen-address=:%d", 9187)
)

func TestAddPGMonitorExporterToInstancePodSpec(t *testing.T) {
	image := "test/image:tag"

	cluster := &v1beta1.PostgresCluster{}
	cluster.Spec.Port = initialize.Int32(5432)
	cluster.Spec.Image = image
	cluster.Spec.ImagePullPolicy = corev1.PullAlways

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		},
	}

	getContainerWithName := func(containers []corev1.Container, name string) corev1.Container {
		for _, container := range containers {
			if container.Name == name {
				return container
			}
		}
		return corev1.Container{}
	}

	t.Run("ExporterDisabled", func(t *testing.T) {
		template := &corev1.PodTemplateSpec{}
		assert.NilError(t, addPGMonitorExporterToInstancePodSpec(cluster, template, nil, nil))
		assert.DeepEqual(t, template, &corev1.PodTemplateSpec{})
	})

	t.Run("ExporterEnabled", func(t *testing.T) {
		cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image:     image,
					Resources: resources,
				},
			},
		}
		template := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: naming.ContainerDatabase,
				}},
			},
		}
		exporterQueriesConfig := &corev1.ConfigMap{
			ObjectMeta: naming.ExporterQueriesConfigMap(cluster),
		}
		assert.NilError(t, addPGMonitorExporterToInstancePodSpec(cluster, template, exporterQueriesConfig, nil))
		container := getContainerWithName(template.Spec.Containers, naming.ContainerPGMonitorExporter)
		assert.Equal(t, container.Image, image)
		assert.Equal(t, container.ImagePullPolicy, corev1.PullAlways)
		assert.DeepEqual(t, container.Resources, resources)
		assert.DeepEqual(t, container.Command, []string{"postgres_exporter", exporterExtendQueryPathFlag, exporterWebListenAddressFlag})
		assert.DeepEqual(t, container.SecurityContext.Capabilities, &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		})
		assert.Equal(t, *container.SecurityContext.Privileged, false)
		assert.Equal(t, *container.SecurityContext.ReadOnlyRootFilesystem, true)
		assert.Equal(t, *container.SecurityContext.AllowPrivilegeEscalation, false)
		assert.Equal(t, *container.Resources.Requests.Cpu(), resource.MustParse("100m"))

		expectedENV := []corev1.EnvVar{
			{Name: "DATA_SOURCE_URI", Value: fmt.Sprintf("localhost:%d/postgres", *cluster.Spec.Port)},
			{Name: "DATA_SOURCE_USER", Value: pgmonitor.MonitoringUser},
			{Name: "DATA_SOURCE_PASS", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: naming.MonitoringUserSecret(cluster).Name,
					},
					Key: "password",
				},
			}}}
		assert.DeepEqual(t, container.Env, expectedENV)

		assert.Assert(t, container.Ports[0].ContainerPort == int32(9187), "Exporter container port number not set to '9187'.")
		assert.Assert(t, container.Ports[0].Name == "exporter", "Exporter container port name not set to 'exporter'.")
		assert.Assert(t, container.Ports[0].Protocol == "TCP", "Exporter container port protocol not set to 'TCP'.")

		assert.Assert(t, template.Spec.Volumes != nil, "No volumes were found.")

		var foundDefaultQueriesVolume bool
		for _, v := range template.Spec.Volumes {
			if v.Name == "exporter-queries" {
				assert.DeepEqual(t, v, corev1.Volume{
					Name: "exporter-queries",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: exporterQueriesConfig.Name,
							},
						},
					},
				})
				foundDefaultQueriesVolume = true
				break
			}
		}
		assert.Assert(t, foundDefaultQueriesVolume, "The default 'exporter-queries' volume was not found.")

		var foundDefaultQueriesMount bool
		for _, vm := range container.VolumeMounts {
			if vm.Name == "exporter-queries" && vm.MountPath == "/opt/crunchy/conf" {
				foundDefaultQueriesMount = true
				break
			}
		}
		assert.Assert(t, foundDefaultQueriesMount, "The default 'exporter-queries' volume mount was not found.")
	})

	t.Run("CustomConfig", func(t *testing.T) {
		cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image:     image,
					Resources: resources,
					Configuration: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "exporter-custom-config-test",
						},
					}},
					},
				},
			},
		}
		template := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: naming.ContainerDatabase,
				}},
			},
		}
		exporterQueriesConfig := &corev1.ConfigMap{
			ObjectMeta: naming.ExporterQueriesConfigMap(cluster),
		}

		assert.NilError(t, addPGMonitorExporterToInstancePodSpec(cluster, template, exporterQueriesConfig, nil))

		var foundConfigVolume bool
		for _, v := range template.Spec.Volumes {
			if v.Name == "exporter-config" {
				assert.DeepEqual(t, v, corev1.Volume{
					Name: "exporter-config",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: cluster.Spec.Monitoring.PGMonitor.Exporter.Configuration,
						},
					},
				})
				foundConfigVolume = true
				break
			}
		}
		assert.Assert(t, foundConfigVolume, "The 'exporter-config' volume was not found.")

		container := getContainerWithName(template.Spec.Containers, naming.ContainerPGMonitorExporter)
		var foundConfigMount bool
		for _, vm := range container.VolumeMounts {
			if vm.Name == "exporter-config" && vm.MountPath == "/conf" {
				foundConfigMount = true
				break
			}
		}
		assert.Assert(t, foundConfigMount, "The 'exporter-config' volume mount was not found.")
	})
}

// TestReconcilePGMonitorExporterSetupErrors tests how reconcilePGMonitorExporter
// reacts when the kubernetes resources are in different states (e.g., checks
// what happens when the database pod is terminating)
func TestReconcilePGMonitorExporterSetupErrors(t *testing.T) {
	for _, test := range []struct {
		name          string
		podExecCalled bool
		status        v1beta1.MonitoringStatus
		monitoring    *v1beta1.MonitoringSpec
		instances     []*Instance
		secret        *corev1.Secret
	}{{
		name:          "Terminating",
		podExecCalled: false,
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "daisy-pod",
						Annotations:       map[string]string{"status": `{"role":"master"}`},
						DeletionTimestamp: &metav1.Time{},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "NotWritable",
		podExecCalled: false,
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-pod",
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "NotRunning",
		podExecCalled: false,
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "ExporterNotRunning",
		podExecCalled: false,
		monitoring: &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image: "image",
				},
			},
		},
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:  naming.ContainerDatabase,
							State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "ExporterImageIDNotFound",
		podExecCalled: false,
		monitoring: &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image: "image",
				},
			},
		},
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:  naming.ContainerDatabase,
							State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						}, {
							Name:  naming.ContainerPGMonitorExporter,
							State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
	}, {
		name:          "NoError",
		podExecCalled: true,
		monitoring: &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image: "image",
				},
			},
		},
		instances: []*Instance{
			{
				Name: "daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:    naming.ContainerDatabase,
							State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
							ImageID: "image@sha123",
						}, {
							Name:    naming.ContainerPGMonitorExporter,
							State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
							ImageID: "image@sha123",
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		},
		secret: &corev1.Secret{
			Data: map[string][]byte{
				"verifier": []byte("blah"),
			},
		},
	}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var called bool
			reconciler := &Reconciler{
				PodExec: func(namespace, pod, container string, stdin io.Reader, stdout,
					stderr io.Writer, command ...string) error {
					called = true
					return nil
				},
			}

			cluster := &v1beta1.PostgresCluster{}
			cluster.Spec.PostgresVersion = 15
			cluster.Spec.Monitoring = test.monitoring
			cluster.Status.Monitoring.ExporterConfiguration = test.status.ExporterConfiguration
			observed := &observedInstances{forCluster: test.instances}

			assert.NilError(t, reconciler.reconcilePGMonitorExporter(ctx,
				cluster, observed, test.secret))
			assert.Equal(t, called, test.podExecCalled)
		})
	}
}

func TestReconcilePGMonitorExporter(t *testing.T) {
	ctx := context.Background()
	var called bool
	reconciler := &Reconciler{
		PodExec: func(namespace, pod, container string, stdin io.Reader, stdout,
			stderr io.Writer, command ...string) error {
			called = true
			return nil
		},
	}

	t.Run("UninstallWhenSecretNil", func(t *testing.T) {
		cluster := &v1beta1.PostgresCluster{}
		cluster.Status.Monitoring.ExporterConfiguration = "installed"
		instances := []*Instance{
			{
				Name: "one-daisy",
				Pods: []*corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "one-daisy-pod",
						Annotations: map[string]string{"status": `{"role":"master"}`},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:    naming.ContainerDatabase,
							ImageID: "dont-care",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						}},
					},
				}},
				Runner: &appsv1.StatefulSet{},
			},
		}
		observed := &observedInstances{forCluster: instances}

		called = false
		assert.NilError(t, reconciler.reconcilePGMonitorExporter(ctx,
			cluster, observed, nil))
		assert.Assert(t, called, "PodExec was not called.")
		assert.Assert(t, cluster.Status.Monitoring.ExporterConfiguration != "", "ExporterConfiguration was empty.")
	})
}

// TestReconcilePGMonitorExporterStatus checks that the exporter status is updated
// when it should be. Because the status updated when we update the setup sql from
// pgmonitor (by using podExec), we check if podExec is called when a change is needed.
func TestReconcilePGMonitorExporterStatus(t *testing.T) {
	for _, test := range []struct {
		name                        string
		exporterEnabled             bool
		podExecCalled               bool
		status                      v1beta1.MonitoringStatus
		statusChangedAfterReconcile bool
	}{{
		name:                        "Disabled",
		podExecCalled:               true,
		statusChangedAfterReconcile: true,
	}, {
		name:                        "Disabled Uninstall",
		podExecCalled:               true,
		status:                      v1beta1.MonitoringStatus{ExporterConfiguration: "installed"},
		statusChangedAfterReconcile: true,
	}, {
		name:                        "Enabled",
		exporterEnabled:             true,
		podExecCalled:               true,
		statusChangedAfterReconcile: true,
	}, {
		name:                        "Enabled Update",
		exporterEnabled:             true,
		podExecCalled:               true,
		status:                      v1beta1.MonitoringStatus{ExporterConfiguration: "installed"},
		statusChangedAfterReconcile: true,
	}, {
		name:            "Enabled NoUpdate",
		exporterEnabled: true,
		podExecCalled:   false,
		// Status was generated manually for this test case
		// TODO jmckulk: add code to generate status
		status:                      v1beta1.MonitoringStatus{ExporterConfiguration: "79b86d7d69"},
		statusChangedAfterReconcile: false,
	}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var (
				called bool
				secret *corev1.Secret
			)

			// Create reconciler with mock PodExec function
			reconciler := &Reconciler{
				PodExec: func(namespace, pod, container string, stdin io.Reader, stdout,
					stderr io.Writer, command ...string) error {
					called = true
					return nil
				},
			}

			// Create the test cluster spec with the exporter status set
			cluster := &v1beta1.PostgresCluster{}
			cluster.Spec.PostgresVersion = 15
			cluster.Status.Monitoring.ExporterConfiguration = test.status.ExporterConfiguration

			// Mock up an instances that will be defined in the cluster. The instances should
			// have all necessary fields that will be needed to reconcile the exporter
			instances := []*Instance{
				{
					Name: "daisy",
					Pods: []*corev1.Pod{{
						ObjectMeta: metav1.ObjectMeta{
							Name:        "daisy-pod",
							Annotations: map[string]string{"status": `{"role":"master"}`},
						},
						Status: corev1.PodStatus{
							ContainerStatuses: []corev1.ContainerStatus{{
								Name:    naming.ContainerDatabase,
								State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
								ImageID: "image@sha123",
							}},
						},
					}},
					Runner: &appsv1.StatefulSet{},
				},
			}

			if test.exporterEnabled {
				// When testing with exporter enabled update the spec with exporter fields
				cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
					PGMonitor: &v1beta1.PGMonitorSpec{
						Exporter: &v1beta1.ExporterSpec{
							Image: "image",
						},
					},
				}

				// Update mock instances to include the exporter container
				instances[0].Pods[0].Status.ContainerStatuses = append(
					instances[0].Pods[0].Status.ContainerStatuses, corev1.ContainerStatus{
						Name:    naming.ContainerPGMonitorExporter,
						State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						ImageID: "image@sha123",
					})

				secret = &corev1.Secret{
					Data: map[string][]byte{
						"verifier": []byte("blah"),
					},
				}
			}

			// Mock up observed instances based on our mock instances
			observed := &observedInstances{forCluster: instances}

			// Check that we can reconcile with the test resources
			assert.NilError(t, reconciler.reconcilePGMonitorExporter(ctx,
				cluster, observed, secret))
			// Check that the exporter status changes when it needs to
			assert.Assert(t, test.statusChangedAfterReconcile == (cluster.Status.Monitoring.ExporterConfiguration != test.status.ExporterConfiguration),
				"got %v", cluster.Status.Monitoring.ExporterConfiguration)
			// Check that pod exec is called correctly
			assert.Equal(t, called, test.podExecCalled)
		})
	}
}

// TestReconcileMonitoringSecret checks that the secret intent returned by reconcileMonitoringSecret
// is correct. If exporter is enabled, the return shouldn't be nil. If the exporter is disabled, the
// return should be nil.
func TestReconcileMonitoringSecret(t *testing.T) {
	// TODO jmckulk: debug test with existing cluster
	// Seems to be an issue when running with other tests
	if strings.EqualFold(os.Getenv("USE_EXISTING_CLUSTER"), "true") {
		t.Skip("Test failing with existing cluster")
	}

	ctx := context.Background()

	// Kubernetes is required because reconcileMonitoringSecret
	// (1) uses the client to get existing secrets
	// (2) sets the controller reference on the new secret
	_, cc := setupKubernetes(t)
	require.ParallelCapacity(t, 0)

	reconciler := &Reconciler{Client: cc, Owner: client.FieldOwner(t.Name())}

	cluster := testCluster()
	cluster.Default()
	cluster.UID = types.UID("hippouid")
	cluster.Namespace = setupNamespace(t, cc).Name

	t.Run("ExporterDisabled", func(t *testing.T) {
		t.Run("NotExisting", func(t *testing.T) {
			secret, err := reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, secret == nil, "Monitoring secret was not nil.")
		})

		t.Run("Existing", func(t *testing.T) {
			cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
				PGMonitor: &v1beta1.PGMonitorSpec{
					Exporter: &v1beta1.ExporterSpec{Image: "image"}}}
			existing, err := reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err, "error in test; existing secret not created")
			assert.Assert(t, existing != nil, "error in test; existing secret not created")

			cluster.Spec.Monitoring = nil
			actual, err := reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, actual == nil, "Monitoring secret still exists after turning exporter off.")
		})
	})

	t.Run("ExporterEnabled", func(t *testing.T) {
		var (
			existing, actual *corev1.Secret
			err              error
		)

		// Enable monitoring in the test cluster spec
		cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image: "image",
				},
			},
		}

		t.Run("NotExisting", func(t *testing.T) {
			existing, err = reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, existing != nil, "Monitoring secret does not exist.")
		})

		t.Run("Existing", func(t *testing.T) {
			actual, err = reconciler.reconcileMonitoringSecret(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, bytes.Equal(actual.Data["password"], existing.Data["password"]), "Passwords do not match.")
		})
	})
}

// TestConfigureExporterTLS checks that tls settings are configured on a podTemplate.
// When exporter is enabled with custom tls configureExporterTLS should add volumes,
// volumeMounts, and a flag to the Command. Ensure that existing template configurations
// are still present.
func TestConfigureExporterTLS(t *testing.T) {
	// Define an existing template with values that could be overwritten
	baseTemplate := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: naming.ContainerPGMonitorExporter,
				Command: []string{
					"postgres_exporter", exporterExtendQueryPathFlag, exporterWebListenAddressFlag,
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "existing-volume",
					MountPath: "some-path",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "existing-volume",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}},
		},
	}

	t.Run("Exporter disabled", func(t *testing.T) {
		cluster := &v1beta1.PostgresCluster{}
		template := baseTemplate.DeepCopy()
		configureExporterTLS(cluster, template, nil)
		// Template shouldn't have changed
		assert.DeepEqual(t, template, baseTemplate)
	})

	t.Run("Exporter enabled no tls", func(t *testing.T) {
		cluster := &v1beta1.PostgresCluster{
			Spec: v1beta1.PostgresClusterSpec{
				Monitoring: &v1beta1.MonitoringSpec{
					PGMonitor: &v1beta1.PGMonitorSpec{
						Exporter: &v1beta1.ExporterSpec{},
					},
				},
			},
		}
		template := baseTemplate.DeepCopy()
		configureExporterTLS(cluster, template, nil)
		// Template shouldn't have changed
		assert.DeepEqual(t, template, baseTemplate)
	})

	t.Run("Custom TLS provided", func(t *testing.T) {
		cluster := &v1beta1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "test"},
			Spec: v1beta1.PostgresClusterSpec{
				Monitoring: &v1beta1.MonitoringSpec{
					PGMonitor: &v1beta1.PGMonitorSpec{
						Exporter: &v1beta1.ExporterSpec{
							CustomTLSSecret: &corev1.SecretProjection{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "custom-exporter-certs",
								},
							},
						},
					},
				},
			},
		}
		template := baseTemplate.DeepCopy()

		testConfigMap := &corev1.ConfigMap{
			ObjectMeta: naming.ExporterWebConfigMap(cluster),
		}

		// What happens if the template already includes volumes/Mounts and envs?
		configureExporterTLS(cluster, template, testConfigMap)

		// Did we configure the cert volume and the web config volume while leaving
		// existing volumes in place?
		assert.Assert(t, marshalMatches(template.Spec.Volumes, `
- emptyDir: {}
  name: existing-volume
- name: exporter-certs
  projected:
    sources:
    - secret:
        name: custom-exporter-certs
- configMap:
    name: test-exporter-web-config
  name: web-config
		`), "Volumes are not what they should be.")

		// Is the exporter container in position 0?
		assert.Assert(t, template.Spec.Containers[0].Name == naming.ContainerPGMonitorExporter,
			"Exporter container is not in the zeroth position.")

		// Did we configure the volume mounts on the container while leaving existing
		// mounts in place?
		assert.Assert(t, marshalMatches(template.Spec.Containers[0].VolumeMounts, `
- mountPath: some-path
  name: existing-volume
- mountPath: /certs
  name: exporter-certs
- mountPath: /web-config
  name: web-config
		`), "Volume mounts are not what they should be.")

		// Did we add the "--web.config.file" flag to the command while leaving the
		// rest intact?
		assert.DeepEqual(t, template.Spec.Containers[0].Command, []string{"postgres_exporter",
			exporterExtendQueryPathFlag, exporterWebListenAddressFlag, "--web.config.file=/web-config/web-config.yml"})
	})
}

// TestReconcileExporterQueriesConfig checks that the ConfigMap intent returned by
// reconcileExporterQueriesConfig is correct. If exporter is enabled, the return
// shouldn't be nil. If the exporter is disabled, the return should be nil.
func TestReconcileExporterQueriesConfig(t *testing.T) {
	ctx := context.Background()

	// Kubernetes is required because reconcileExporterQueriesConfig
	// (1) uses the client to get existing ConfigMaps
	// (2) sets the controller reference on the new ConfigMap
	_, cc := setupKubernetes(t)
	require.ParallelCapacity(t, 0)

	reconciler := &Reconciler{Client: cc, Owner: client.FieldOwner(t.Name())}

	cluster := testCluster()
	cluster.Default()
	cluster.UID = types.UID("hippouid")
	cluster.Namespace = setupNamespace(t, cc).Name

	t.Run("ExporterDisabled", func(t *testing.T) {
		t.Run("NotExisting", func(t *testing.T) {
			queriesConfig, err := reconciler.reconcileExporterQueriesConfig(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, queriesConfig == nil, "Default queries ConfigMap is present.")
		})

		t.Run("Existing", func(t *testing.T) {
			cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
				PGMonitor: &v1beta1.PGMonitorSpec{
					Exporter: &v1beta1.ExporterSpec{Image: "image"}}}
			existing, err := reconciler.reconcileExporterQueriesConfig(ctx, cluster)
			assert.NilError(t, err, "error in test; existing config not created")
			assert.Assert(t, existing != nil, "error in test; existing config not created")

			cluster.Spec.Monitoring = nil
			actual, err := reconciler.reconcileExporterQueriesConfig(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, actual == nil, "Default queries config still present after disabling exporter.")
		})
	})

	t.Run("ExporterEnabled", func(t *testing.T) {
		var (
			existing, actual *corev1.ConfigMap
			err              error
		)

		// Enable monitoring in the test cluster spec
		cluster.Spec.Monitoring = &v1beta1.MonitoringSpec{
			PGMonitor: &v1beta1.PGMonitorSpec{
				Exporter: &v1beta1.ExporterSpec{
					Image: "image",
				},
			},
		}

		t.Run("NotExisting", func(t *testing.T) {
			existing, err = reconciler.reconcileExporterQueriesConfig(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, existing != nil, "Default queries config does not exist.")
		})

		t.Run("Existing", func(t *testing.T) {
			actual, err = reconciler.reconcileExporterQueriesConfig(ctx, cluster)
			assert.NilError(t, err)
			assert.Assert(t, actual.Data["queries.yml"] == existing.Data["queries.yml"], "Data does not align.")
		})
	})
}

func TestGenerateQueries(t *testing.T) {
	ctx := context.Background()
	cluster := &v1beta1.PostgresCluster{}

	t.Run("PG<=11", func(t *testing.T) {
		cluster.Spec.PostgresVersion = 11
		queries := generateQueries(ctx, cluster)
		assert.Assert(t, !strings.Contains(queries, "ccp_pg_stat_statements_reset"),
			"Queries contain 'ccp_pg_stat_statements_reset' query when they should not.")
	})

	t.Run("PG>=12", func(t *testing.T) {
		cluster.Spec.PostgresVersion = 12
		queries := generateQueries(ctx, cluster)
		assert.Assert(t, strings.Contains(queries, "ccp_pg_stat_statements_reset"),
			"Queries do not contain 'ccp_pg_stat_statements_reset' query when they should.")
	})
}
