// Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package metricsserver

import (
	"context"
	"fmt"

	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/operation/botanist/component"
	"github.com/gardener/gardener/pkg/utils"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/gardener/gardener/pkg/utils/managedresources"
	"github.com/gardener/gardener/pkg/utils/secrets"

	resourcesv1alpha1 "github.com/gardener/gardener-resource-manager/api/resources/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ManagedResourceName is the name of the ManagedResource containing the resource specifications.
	ManagedResourceName = "shoot-core-metrics-server"
	// SecretNameCA is the name of the secret containing the CA certificate and key for the metrics-server.
	SecretNameCA = v1beta1constants.SecretNameCAMetricsServer
	// SecretNameServer is the name of the secret containing the server certificate and key for the metrics-server.
	SecretNameServer = "metrics-server"

	deploymentName     = "metrics-server"
	serviceName        = "metrics-server"
	serviceAccountName = "metrics-server"
	containerName      = "metrics-server"
	addonResizerName   = "metrics-server-nanny"

	servicePort   int32 = 443
	containerPort int32 = 8443

	volumeMountNameServer = "metrics-server"
	volumeMountPathServer = "/srv/metrics-server/tls"
)

// Interface contains functions for a metrics-server deployer.
type Interface interface {
	component.DeployWaiter
	// SetSecrets sets the secrets.
	SetSecrets(Secrets)
	// ServiceDNSNames returns the service DNS names for the metrics-server.
	ServiceDNSNames() []string
}

// New creates a new instance of DeployWaiter for the metrics-server.
func New(
	client client.Client,
	namespace string,
	image string,
	imageAddonResizer string,
	kubeAPIServerHost *string,
) Interface {
	return &metricsServer{
		client:            client,
		namespace:         namespace,
		image:             image,
		kubeAPIServerHost: kubeAPIServerHost,
		imageAddonResizer: imageAddonResizer,
	}
}

type metricsServer struct {
	client            client.Client
	namespace         string
	image             string
	imageAddonResizer string
	kubeAPIServerHost *string
	secrets           Secrets
}

func (m *metricsServer) Deploy(ctx context.Context) error {
	if m.secrets.CA.Name == "" || m.secrets.CA.Checksum == "" {
		return fmt.Errorf("missing CA secret information")
	}
	if m.secrets.Server.Name == "" || m.secrets.Server.Checksum == "" {
		return fmt.Errorf("missing server secret information")
	}

	data, err := m.computeResourcesData()
	if err != nil {
		return err
	}

	return managedresources.CreateForShoot(ctx, m.client, m.namespace, ManagedResourceName, false, data)
}

func (m *metricsServer) Destroy(ctx context.Context) error {
	return managedresources.DeleteForShoot(ctx, m.client, m.namespace, ManagedResourceName)
}

func (m *metricsServer) Wait(_ context.Context) error        { return nil }
func (m *metricsServer) WaitCleanup(_ context.Context) error { return nil }
func (m *metricsServer) SetSecrets(secrets Secrets)          { m.secrets = secrets }

func (m *metricsServer) ServiceDNSNames() []string {
	return append(
		[]string{serviceName},
		kutil.DNSNamesForService(serviceName, metav1.NamespaceSystem)...,
	)
}

func (m *metricsServer) computeResourcesData() (map[string][]byte, error) {
	var (
		registry = managedresources.NewRegistry(kubernetes.ShootScheme, kubernetes.ShootCodec, kubernetes.ShootSerializer)

		serviceAccount = &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceAccountName,
				Namespace: metav1.NamespaceSystem,
			},
		}

		clusterRole = &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system:metrics-server",
			},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{""},
					Resources: []string{"pods", "nodes", "nodes/stats", "namespaces", "configmaps"},
					Verbs:     []string{"get", "list", "watch"},
				},
				{
					APIGroups: []string{"apps"},
					Resources: []string{"deployments"},
					Verbs:     []string{"get", "list", "update", "watch"},
				},
			},
		}

		clusterRoleBinding = &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system:metrics-server",
				Annotations: map[string]string{
					resourcesv1alpha1.DeleteOnInvalidUpdate: "true",
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     clusterRole.Name,
			},
			Subjects: []rbacv1.Subject{{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      serviceAccount.Name,
				Namespace: serviceAccount.Namespace,
			}},
		}

		clusterRoleBindingAuthDelegator = &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "metrics-server:system:auth-delegator",
				Annotations: map[string]string{
					resourcesv1alpha1.DeleteOnInvalidUpdate: "true",
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     "system:auth-delegator",
			},
			Subjects: []rbacv1.Subject{{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      serviceAccount.Name,
				Namespace: serviceAccount.Namespace,
			}},
		}

		roleBinding = &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "metrics-server-auth-reader",
				Namespace: metav1.NamespaceSystem,
				Annotations: map[string]string{
					resourcesv1alpha1.DeleteOnInvalidUpdate: "true",
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "Role",
				Name:     "extension-apiserver-authentication-reader",
			},
			Subjects: []rbacv1.Subject{{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      serviceAccount.Name,
				Namespace: serviceAccount.Namespace,
			}},
		}

		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "metrics-server",
				Namespace: metav1.NamespaceSystem,
			},
			Type: corev1.SecretTypeTLS,
			Data: m.secrets.Server.Data,
		}

		service = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: metav1.NamespaceSystem,
				Labels:    map[string]string{"kubernetes.io/name": serviceName},
			},
			Spec: corev1.ServiceSpec{
				Selector: getLabels(),
				Ports: []corev1.ServicePort{
					{
						Port:       servicePort,
						Protocol:   corev1.ProtocolTCP,
						TargetPort: intstr.FromInt(int(containerPort)),
					},
				},
			},
		}

		apiService = &apiregistrationv1.APIService{
			ObjectMeta: metav1.ObjectMeta{
				Name: "v1beta1.metrics.k8s.io",
			},
			Spec: apiregistrationv1.APIServiceSpec{
				Service: &apiregistrationv1.ServiceReference{
					Name:      service.Name,
					Namespace: metav1.NamespaceSystem,
				},
				Group:                "metrics.k8s.io",
				GroupPriorityMinimum: 100,
				Version:              "v1beta1",
				VersionPriority:      100,
				CABundle:             m.secrets.CA.Data[secrets.DataKeyCertificateCA],
			},
		}

		maxUnavailable = intstr.FromInt(0)
		deployment     = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deploymentName,
				Namespace: metav1.NamespaceSystem,
				Labels: utils.MergeStringMaps(getLabels(), map[string]string{
					managedresources.LabelKeyOrigin: managedresources.LabelValueGardener,
					v1beta1constants.GardenRole:     v1beta1constants.GardenRoleSystemComponent,
				}),
				Annotations: map[string]string{
					resourcesv1alpha1.PreserveResources: "true",
				},
			},
			Spec: appsv1.DeploymentSpec{
				RevisionHistoryLimit: pointer.Int32(1),
				Selector:             &metav1.LabelSelector{MatchLabels: getLabels()},
				Strategy: appsv1.DeploymentStrategy{
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxUnavailable: &maxUnavailable,
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: utils.MergeStringMaps(getLabels(), map[string]string{
							managedresources.LabelKeyOrigin:                     managedresources.LabelValueGardener,
							v1beta1constants.GardenRole:                         v1beta1constants.GardenRoleSystemComponent,
							v1beta1constants.LabelNetworkPolicyShootFromSeed:    v1beta1constants.LabelNetworkPolicyAllowed,
							v1beta1constants.LabelNetworkPolicyShootToAPIServer: v1beta1constants.LabelNetworkPolicyAllowed,
							v1beta1constants.LabelNetworkPolicyShootToKubelet:   v1beta1constants.LabelNetworkPolicyAllowed,
							v1beta1constants.LabelNetworkPolicyToDNS:            v1beta1constants.LabelNetworkPolicyAllowed,
						}),
						Annotations: map[string]string{
							"scheduler.alpha.kubernetes.io/critical-pod": "",
							"checksum/secret-" + secret.Name:             m.secrets.Server.Checksum,
						},
					},
					Spec: corev1.PodSpec{
						Tolerations: []corev1.Toleration{{
							Key:      "CriticalAddonsOnly",
							Operator: corev1.TolerationOpExists,
						}},
						PriorityClassName: "system-cluster-critical",
						NodeSelector: map[string]string{
							v1beta1constants.LabelWorkerPoolSystemComponents: "true",
						},
						SecurityContext: &corev1.PodSecurityContext{
							RunAsUser: pointer.Int64(65534),
							FSGroup:   pointer.Int64(65534),
						},
						DNSPolicy:          corev1.DNSDefault, // make sure to not use the coredns for DNS resolution.
						ServiceAccountName: serviceAccount.Name,
						Containers: []corev1.Container{{
							Name:            containerName,
							Image:           m.image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"/metrics-server",
								"--authorization-always-allow-paths=/livez,/readyz",
								"--profiling=false",
								// nobody user only can write in home folder
								"--cert-dir=/home/certdir",
								fmt.Sprintf("--secure-port=%d", containerPort),
								// See https://github.com/kubernetes-incubator/metrics-server/issues/25 and https://github.com/kubernetes-incubator/metrics-server/issues/130
								// The kube-apiserver and the kubelet use different CAs, however, the metrics-server assumes the CAs are the same.
								// We should remove this flag once it is possible to specify the CA of the kubelet.
								"--kubelet-insecure-tls",
								"--kubelet-preferred-address-types=[Hostname,InternalDNS,InternalIP,ExternalDNS,ExternalIP]",
								fmt.Sprintf("--tls-cert-file=%s/%s", volumeMountPathServer, secrets.DataKeyCertificate),
								fmt.Sprintf("--tls-private-key-file=%s/%s", volumeMountPathServer, secrets.DataKeyPrivateKey),
							},
							ReadinessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/readyz",
										Port:   intstr.FromInt(int(containerPort)),
										Scheme: corev1.URISchemeHTTPS,
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
								FailureThreshold:    1,
							},
							LivenessProbe: &corev1.Probe{
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/livez",
										Port:   intstr.FromInt(int(containerPort)),
										Scheme: corev1.URISchemeHTTPS,
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       30,
								FailureThreshold:    1,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("150Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{{
								Name:      volumeMountNameServer,
								MountPath: volumeMountPathServer,
							}},
						}, {
							Name:            addonResizerName,
							Image:           m.imageAddonResizer,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"/pod_nanny",
								"--cpu=20m",
								"--extra-cpu=1m",
								"--memory=15Mi",
								"--extra-memory=2Mi",
								"--threshold=5",
								"--deployment=" + deploymentName,
								"--container=" + containerName,
								"--poll-period=300000",
								"--minClusterSize=10",
								"--use-metrics=false",
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("40m"),
									corev1.ResourceMemory: resource.MustParse("25Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("40m"),
									corev1.ResourceMemory: resource.MustParse("25Mi"),
								},
							},
							Env: []corev1.EnvVar{{
								Name: "MY_POD_NAME",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.name",
									},
								},
							}, {
								Name: "MY_POD_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							}},
						}},
						Volumes: []corev1.Volume{{
							Name: volumeMountNameServer,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: secret.Name,
								},
							},
						}},
					},
				},
			},
		}
	)

	if m.kubeAPIServerHost != nil {
		deployment.Spec.Template.Spec.Containers[0].Env = append(deployment.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
			Name:  "KUBERNETES_SERVICE_HOST",
			Value: *m.kubeAPIServerHost,
		})
	}

	return registry.AddAllAndSerialize(
		serviceAccount,
		clusterRole,
		clusterRoleBinding,
		clusterRoleBindingAuthDelegator,
		roleBinding,
		secret,
		service,
		apiService,
		deployment,
	)
}

func getLabels() map[string]string {
	return map[string]string{"k8s-app": "metrics-server"}
}

// Secrets is collection of secrets for the metrics-server.
type Secrets struct {
	// CA is a secret containing the CA certificate and key.
	CA component.Secret
	// Server is a secret containing the server certificate and key.
	Server component.Secret
}
